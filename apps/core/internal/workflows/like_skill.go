// like_skill.go：like 能力的 Skill 网关执行链（M3-2，工作项 wi-1795707934-8748）。
//
// M3-2 把 pyreplay 的点赞能力收口成 skill 'like'（services/like_skill.py + skill.json），
// Go 侧原来的两条链路随之重写为**一条**：
//
//   - 旧 LIKE_VIA_SKILL=true 分支：workflow 内逐条 ExecuteActivity("InvokeSkill", "like", …)，
//     每条之间 workflow.Sleep 3s/5s；
//   - 旧 pyreplay worker 分支：ExecuteActivity("LikeActivities.like_batch", …) 直连 Python worker。
//
// 现在统一走 LikeSkillWorkflow → ExecuteActivity("InvokeSkill", …) → skillgateway →
// pyreplay 的 like 技能。技能侧一次调用 = 取账号 + 起浏览器会话 + 逐次点赞（秒级到几十秒），
// 账号冷却与多次点赞之间的抖动都在技能内部完成，所以 workflow 层不再自己 sleep：
// 编排层只负责编排，节奏交给技能侧的反封号策略。
package workflows

import (
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/skillcontract"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// SkillLikeName 是点赞能力的 skill 名（apps/pyreplay/skill.json 的 name）。
const SkillLikeName = "like"

// likeBatchLimit 是单次 workflow 最多处理的待赞推文数（与 activities.GetPendingLikes
// 的缺省口径一致；workflow 显式带上它，避免两层各写一套默认值）。
const likeBatchLimit = 20

// likeRetryInitialInterval 是点赞技能调用的首次重试退避。
//
// 取 60s 不是随手写的：技能侧一次调用 = 账号轮换 + 起浏览器 + 逐次点赞，秒级抖动
// 没有意义；而 skillgateway 对 like 限流 rate_per_min=30、并发 max_concurrency=1，
// 429（rejected，不落熔断）与「连不上服务」这类传输失败一样由 Temporal 重试兜底，
// 退避太短只会让整批点赞卡在同一个拥塞窗口里空转。
const likeRetryInitialInterval = 60 * time.Second

// LikeSkillInput 是 skill 化点赞 workflow 的输入。
type LikeSkillInput struct {
	// Source 是 raw_items.source（缺省 x）。
	Source string `json:"source"`
	// Keyword 非空时只取该关键词入库的推文。
	Keyword string `json:"keyword"`
	// Limit 是单次最多处理多少条待赞推文（<=0 时用 likeBatchLimit）。
	Limit int `json:"limit"`
	// LikeCount 是每条推文的点赞次数（同一推文用不同账号各点一次），
	// 取值口径由 skillcontract.ClampLikeCount 规整到 [1, 20]。
	LikeCount int `json:"like_count"`
}

// LikeSkillResult 报告本次点赞分发的结果（逐条明细供 API 层直接回给调用方）。
type LikeSkillResult struct {
	Total   int            `json:"total"`
	Success int            `json:"success"`
	Failed  int            `json:"failed"`
	Results []LikeItemLike `json:"results"`
}

// LikeItemLike 是单条推文的点赞结果。
type LikeItemLike struct {
	TweetID  string `json:"tweet_id"`
	URL      string `json:"url"`
	Success  bool   `json:"success"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}

// LikeSkillWorkflow 取待赞推文 → 逐条调用 skill 'like' → 成功项回写 raw_items.liked。
//
// 单条推文点赞失败不阻断整批（计入 failed 并继续），批次层面的 DB 读写失败才返回错误；
// 成功集非空时一次性回写，避免逐条 UPDATE 打爆业务库。
func LikeSkillWorkflow(ctx workflow.Context, input LikeSkillInput) (LikeSkillResult, error) {
	source := input.Source
	if source == "" {
		source = db.PlatformX
	}
	if input.Limit <= 0 {
		input.Limit = likeBatchLimit
	}
	// like_count 在入口一次性规整（口径与技能侧 clamp_like_count 完全一致，契约测试钉住）：
	// 逐条点赞用的都是它，越界值在编排层就定形，不会到技能侧被静默改成另一个数。
	likeCount := skillcontract.ClampLikeCount(input.LikeCount)

	// 取数 / 回写是本地 Go activity（直连业务库）：给超时 + 有界重试（5 次快退避）。
	// 不给 MaximumAttempts 时 Temporal 用的是「无限重试」，一次真故障会让 workflow 卡在
	// activity 上直到 workflow 超时，board 上表现为整批点赞长期不返回。
	dbCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    5,
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
		},
	})

	var pending activities.GetPendingLikesResult
	err := workflow.ExecuteActivity(dbCtx, "GetPendingLikes", activities.GetPendingLikesInput{
		Source:  source,
		Keyword: input.Keyword,
		Limit:   input.Limit,
	}).Get(ctx, &pending)
	if err != nil {
		return LikeSkillResult{}, err
	}

	result := LikeSkillResult{Total: len(pending.Items)}
	if len(pending.Items) == 0 {
		return result, nil
	}

	// skill 单次最长 limits.timeout_sec（like = 180s：账号轮换 + 每次尝试都要起浏览器），
	// activity 超时留 5 分钟余量；失败重试 3 次、首次退避见 likeRetryInitialInterval。
	skillCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    3,
			InitialInterval:    likeRetryInitialInterval,
			BackoffCoefficient: 2.0,
			MaximumInterval:    5 * time.Minute,
		},
	})

	likedIDs := make([]string, 0, len(pending.Items))
	for _, item := range pending.Items {
		itemResult := LikeItemLike{TweetID: item.ExternalID, URL: item.URL}

		if item.URL == "" {
			itemResult.Error = "raw item has no url"
			result.Failed++
			result.Results = append(result.Results, itemResult)
			continue
		}

		out, invokeErr := callLikeSkill(skillCtx, item.URL, likeCount)
		if invokeErr != nil {
			// 走到这里说明是调用级失败（网关 5xx / 限流 / 网络 / 输出不合契约）：
			// 技能自报的业务失败在网关侧就被包成这类错误，具体原因由下面拼出的入参 +
			// 网关返回体一起给出，board 上能直接看出送出去的是哪个 URL、点几次。
			itemResult.Error = fmt.Sprintf("invoke skill %s(post_url=%s, like_count=%d): %v",
				SkillLikeName, item.URL, likeCount, invokeErr)
			result.Failed++
			result.Results = append(result.Results, itemResult)
			continue
		}

		contract := skillcontract.Output(out)
		itemResult.Attempts = contract.Int("attempts", 0)
		itemResult.Success = contract.OK()
		if itemResult.Success {
			result.Success++
			if id := contract.Str("tweet_id"); id != "" {
				itemResult.TweetID = id
			}
			likedIDs = append(likedIDs, itemResult.TweetID)
		} else {
			itemResult.Error = contract.Reason()
			result.Failed++
		}
		result.Results = append(result.Results, itemResult)
	}

	// 回写只处理技能回报成功的项（一次 UPDATE，不逐条打业务库）。
	if len(likedIDs) > 0 {
		err := workflow.ExecuteActivity(dbCtx, "MarkRawItemsLiked", activities.MarkRawItemsLikedInput{
			Source:      source,
			ExternalIDs: likedIDs,
		}).Get(ctx, nil)
		if err != nil {
			// 点赞动作本身已经发生（成功项已计入 result），回写失败对调用方是「半成功」：
			// raw_items.liked 没落库，下一轮会重复分发，由技能侧 like_records 的账号级去重兜底。
			// 计数写进错误文本：workflow 以 activity 错误终止时结果 payload 不可用（调用方
			// wr.Get(err) 读不到），「点了几个、没标记成」只能靠这句话在 API 响应里落地。
			return result, fmt.Errorf("mark %d liked tweet(s) failed: %w", result.Success, err)
		}
	}
	return result, nil
}

// callLikeSkill 经 Skill 网关调用 like 技能并取回输出对象（单条推文一次调用）。
//
// ctx 必须传带 ActivityOptions 的那个 context（超时与重试都在里面）；输入由
// skillcontract.LikeInput 构造：字段名与 like_count 的取值口径都在契约层收口，与
// apps/pyreplay/skill.json 逐字对齐（skillgateway 侧还会按 input_schema 再校验一次）。
func callLikeSkill(ctx workflow.Context, postURL string, likeCount int) (map[string]any, error) {
	var out map[string]any
	input := skillcontract.LikeInput(postURL, likeCount).Map()
	if err := workflow.ExecuteActivity(ctx, "InvokeSkill", SkillLikeName, input).Get(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
