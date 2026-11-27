package workflows

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/skillcontract"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// likeTestEnv 是 like 技能链 workflow 的测试台（Temporal testsuite 的薄封装）。
// gatewayFail 里的 post_url 会让 InvokeSkill 返回调用级错误（等价于 skillgateway 的
// HTTP 5xx / 网络失败，而不是技能回报的业务失败）。
type likeTestEnv struct {
	suite       testsuite.WorkflowTestSuite
	env         *testsuite.TestWorkflowEnvironment
	gatewayFail map[string]bool
}

// markRecorder 记录 MarkRawItemsLiked 收到的回写集合（断言「只回写成功项」）。
type markRecorder struct {
	mu    sync.Mutex
	ids   []string
	calls int
}

func (r *markRecorder) add(ids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.ids = append(r.ids, ids...)
}

func (r *markRecorder) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.ids, ",")
}

func (r *markRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// outcome 是一条推文的模拟点赞结果（skill 'like' output_schema 的子集）。
type outcome struct {
	ok       bool
	reason   string
	tweetID  string
	success  int
	accounts []int
}

// newLikeTestEnv 建测试台：pending = 待赞推文集（含取数错误），skills = 按 URL 的点赞结果，
// markErr = 回写库错误；返回测试台与回写记录器。
func newLikeTestEnv(
	t *testing.T,
	pending []db.RawItem,
	pendingErr error,
	skills map[string]outcome,
	markErr error,
) (*likeTestEnv, *markRecorder) {
	t.Helper()

	env := &likeTestEnv{gatewayFail: map[string]bool{}}
	env.env = env.suite.NewTestWorkflowEnvironment()
	// 点赞技能的重试退避是 60s 起（见 likeRetryInitialInterval），失败路径全靠虚拟时间
	// 快进跑完；不设这一句的话测试会按真实墙上时钟真等满退避（生产参数 × 重试次数，
	// 一个用例要跑十几分钟）。
	env.env.SetWorkflowRunTimeout(0)
	env.env.SetActivityTimeoutGracePeriod(0)
	rec := &markRecorder{}

	getPending := func(_ context.Context, input activities.GetPendingLikesInput) (activities.GetPendingLikesResult, error) {
		if pendingErr != nil {
			return activities.GetPendingLikesResult{}, pendingErr
		}
		// source 缺省由 workflow 补成 x；这里校验它一定非空（否则说明编排漏了默认值）
		if input.Source == "" {
			t.Error("GetPendingLikes 收到空 source：workflow 未填默认平台")
		}
		return activities.GetPendingLikesResult{Items: pending}, nil
	}

	invokeSkill := func(_ context.Context, name string, input map[string]any) (map[string]any, error) {
		if name != SkillLikeName {
			t.Errorf("InvokeSkill skill = %q, want %q", name, SkillLikeName)
		}
		postURL, _ := input[skillcontract.FieldPostURL].(string)
		count := toInt(input[skillcontract.FieldLikeCount])
		if count < skillcontract.LikeCountDefault || count > skillcontract.LikeCountMax {
			t.Errorf("InvokeSkill 输入 like_count=%v 不在 [%d, %d]（skill schema 越界）",
				input[skillcontract.FieldLikeCount], skillcontract.LikeCountDefault, skillcontract.LikeCountMax)
		}
		if env.gatewayFail[postURL] {
			return nil, errors.New("invoke skill like: HTTP 502: skill HTTP 502: bad gateway")
		}
		got, found := skills[postURL]
		if !found {
			t.Errorf("InvokeSkill 收到未预期的 post_url=%q (like_count=%d)", postURL, count)
			return map[string]any{skillcontract.FieldOK: false, skillcontract.FieldReason: "unexpected url"}, nil
		}
		return map[string]any{
			skillcontract.FieldOK:     got.ok,
			skillcontract.FieldReason: got.reason,
			"tweet_id":                got.tweetID,
			"attempts":                count,
			"success_count":           got.success,
			"account_ids":             got.accounts,
		}, nil
	}

	markLiked := func(_ context.Context, input activities.MarkRawItemsLikedInput) error {
		if input.Source == "" {
			t.Error("MarkRawItemsLiked 收到空 source")
		}
		rec.add(input.ExternalIDs)
		return markErr
	}

	// activity 按名字注册：workflow 侧是按名字调度的（ExecuteActivity(ctx, "GetPendingLikes", …)），
	// 这里必须是同一组名字，否则 mock 永远打不中、用例会在超时后失败。
	env.env.RegisterActivityWithOptions(getPending, activity.RegisterOptions{Name: "GetPendingLikes"})
	env.env.RegisterActivityWithOptions(invokeSkill, activity.RegisterOptions{Name: "InvokeSkill"})
	env.env.RegisterActivityWithOptions(markLiked, activity.RegisterOptions{Name: "MarkRawItemsLiked"})
	return env, rec
}

// run 执行 LikeSkillWorkflow 并返回 (结果, workflow 错误)。
//
// 用 ExecuteWorkflow(fn, args...) 按真实签名注册 workflow：workflow 入参与 activity 入参/
// 出参都会经 Temporal 的 JSON 编解码往返一遍，字段名写错在测试里就暴露。
// workflow 以 activity 错误终止时结果 payload 不可用（与生产侧 wr.Get(err) 同口径），
// 需要断言半成功计数的用例请断言包装后的错误文本。
func (e *likeTestEnv) run(input LikeSkillInput) (LikeSkillResult, error) {
	// TestTimeout 是空闲看门狗（连续空转那么久即判卡死），不是执行预算；显式给短值，
	// 让「workflow 卡在某个 activity 上永远不返回」这类问题在测试里秒级暴露。
	e.env.SetTestTimeout(10 * time.Second)
	e.env.ExecuteWorkflow(LikeSkillWorkflow, input)
	var result LikeSkillResult
	err := e.env.GetWorkflowError()
	if resErr := e.env.GetWorkflowResult(&result); resErr != nil && err == nil {
		err = resErr
	}
	return result, err
}

// toInt 读取 skill 输入里的整数字段：activity 入参经 Temporal 的 JSON 编解码往返，
// Go 侧的 int 到 mock 这一侧会变成 float64（网关侧则是 json.Number），断言前统一收口。
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}

const (
	likeURL1 = "https://x.com/a/status/1"
	likeURL2 = "https://x.com/b/status/2"
)

// TestLikeSkillWorkflowSuccess 钉住 M3-2 的 like 主链路：
// GetPendingLikes → 逐条 InvokeSkill("like", {post_url, like_count}) → MarkRawItemsLiked。
//
// 走的是 Skill 网关（InvokeSkill），不再直连 pyreplay 的 LikeActivities.*；传给技能的输入
// 符合 skill 'like' 的 schema（由技能 mock 就地校验字段名与取值区间）；回写用的 tweet_id
// 以技能回报为准（不是 raw_items.external_id），且整批只回写一次。
func TestLikeSkillWorkflowSuccess(t *testing.T) {
	env, marks := newLikeTestEnv(t,
		[]db.RawItem{
			{Source: db.PlatformX, ExternalID: "x_1", URL: likeURL1},
			{Source: db.PlatformX, ExternalID: "x_2", URL: likeURL2},
		},
		nil,
		map[string]outcome{
			likeURL1: {ok: true, tweetID: "1", success: 3, accounts: []int{7}},
			likeURL2: {ok: true, tweetID: "2", success: 1, accounts: []int{8}},
		},
		nil,
	)

	result, err := env.run(LikeSkillInput{Keyword: "ai", Limit: 5, LikeCount: 3})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if result.Total != 2 || result.Success != 2 || result.Failed != 0 {
		t.Fatalf("result = %+v, want total=2 success=2 failed=0", result)
	}
	if got := marks.joined(); got != "1,2" {
		t.Fatalf("回写的 tweet ids = %q, want 1,2", got)
	}
	if marks.count() != 1 {
		t.Fatalf("MarkRawItemsLiked 调用次数 = %d, want 1（成功集一次性回写）", marks.count())
	}
	if len(result.Results) != 2 || result.Results[0].Attempts != 3 {
		t.Fatalf("results = %+v, want 2 条且首条 attempts=3", result.Results)
	}
}

// TestLikeSkillWorkflowLikeCountClamped 验证 like_count 越界被规整到 [1, 20]：越界值会
// 以「InvokeSkill 输入越界」在技能 mock 里报错（t.Errorf），测试随即失败。
func TestLikeSkillWorkflowLikeCountClamped(t *testing.T) {
	env, marks := newLikeTestEnv(t,
		[]db.RawItem{{Source: db.PlatformX, ExternalID: "x_1", URL: likeURL1}},
		nil,
		map[string]outcome{likeURL1: {ok: true, tweetID: "1", success: 20}},
		nil,
	)

	result, err := env.run(LikeSkillInput{LikeCount: 999})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if result.Success != 1 || result.Results[0].Attempts != skillcontract.LikeCountMax {
		t.Fatalf("result = %+v, want attempts=%d", result.Results, skillcontract.LikeCountMax)
	}
	// 回写 activity 按 dbCtx 的有界重试跑了 MaximumAttempts 次（都失败）：断言「尝试过且
	// 只带成功项」，不钉次数——次数是重试策略的表达，归编排层（dbCtx）负责。
	if marks.count() == 0 || marks.joined() != strings.TrimSuffix(strings.Repeat("1,", marks.count()), ",") {
		t.Fatalf("回写 = %q (calls=%d), want 只带成功项 1", marks.joined(), marks.count())
	}
}

// TestLikeSkillWorkflowPartialFailure 验证单条失败不阻断整批：失败条计入 failed 并带原因，
// 无 URL 的条目连技能调用都不发；只有成功项进回写集合。
func TestLikeSkillWorkflowPartialFailure(t *testing.T) {
	env, marks := newLikeTestEnv(t,
		[]db.RawItem{
			{Source: db.PlatformX, ExternalID: "x_1", URL: likeURL1},
			{Source: db.PlatformX, ExternalID: "x_2", URL: ""},
			{Source: db.PlatformX, ExternalID: "x_3", URL: likeURL2},
		},
		nil,
		map[string]outcome{
			likeURL1: {ok: false, reason: "all like attempts failed", tweetID: "1"},
			likeURL2: {ok: true, tweetID: "2", success: 1},
		},
		nil,
	)

	result, err := env.run(LikeSkillInput{})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if result.Total != 3 || result.Success != 1 || result.Failed != 2 {
		t.Fatalf("result = %+v, want total=3 success=1 failed=2", result)
	}
	if len(result.Results) != 3 {
		t.Fatalf("results 条数 = %d, want 3", len(result.Results))
	}
	if result.Results[0].Error != "all like attempts failed" {
		t.Fatalf("首条失败原因 = %q", result.Results[0].Error)
	}
	if !strings.Contains(result.Results[1].Error, "no url") {
		t.Fatalf("无 URL 条目的原因 = %q, want 含 no url", result.Results[1].Error)
	}
	if got := marks.joined(); got != "2" {
		t.Fatalf("回写 = %q, want 2（失败项不回写）", got)
	}
}

// TestLikeSkillWorkflowGatewayError 验证技能调用整体失败（网关 5xx）时该条计入 failed，
// 错误信息原样带给调用方，且不影响同批其它推文。
func TestLikeSkillWorkflowGatewayError(t *testing.T) {
	env, marks := newLikeTestEnv(t,
		[]db.RawItem{
			{Source: db.PlatformX, ExternalID: "x_1", URL: likeURL1},
			{Source: db.PlatformX, ExternalID: "x_2", URL: likeURL2},
		},
		nil,
		map[string]outcome{likeURL2: {ok: true, tweetID: "2", success: 1}},
		nil,
	)

	// 第一条走网关失败（调用级错误，等价于网关 5xx / 限流 / 连不上服务），第二条照常成功。
	env.gatewayFail[likeURL1] = true

	result, err := env.run(LikeSkillInput{Source: db.PlatformX})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if result.Total != 2 || result.Success != 1 || result.Failed != 1 {
		t.Fatalf("result = %+v, want total=2 success=1 failed=1", result)
	}
	if !strings.Contains(result.Results[0].Error, "HTTP 502") {
		t.Fatalf("网关失败原因未透传: %q", result.Results[0].Error)
	}
	if got := marks.joined(); got != "2" {
		t.Fatalf("回写 = %q, want 2", got)
	}
}

// TestLikeSkillWorkflowEmpty 验证没有待赞推文时：不调技能、不回写、结果全零。
func TestLikeSkillWorkflowEmpty(t *testing.T) {
	env, marks := newLikeTestEnv(t, nil, nil, nil, nil)

	result, err := env.run(LikeSkillInput{Source: db.PlatformX})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if result.Total != 0 || result.Success != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want 全零", result)
	}
	if marks.count() != 0 {
		t.Fatalf("空批次不该回写 raw_items（calls=%d）", marks.count())
	}
}

// TestLikeSkillWorkflowPendingError 验证取数失败时 workflow 原样报错（不静默当空批）。
func TestLikeSkillWorkflowPendingError(t *testing.T) {
	env, marks := newLikeTestEnv(t, nil, errors.New("db down"), nil, nil)

	if _, err := env.run(LikeSkillInput{}); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("workflow err = %v, want 含 db down", err)
	}
	if marks.count() != 0 {
		t.Fatal("取数失败时不该回写")
	}
}

// TestLikeSkillWorkflowMarkError 验证回写失败时 workflow 报错，且错误里带出成功计数：
// 点赞动作已经发生（技能侧 like_records 有记录），只是 raw_items.liked 没落库，下一轮会
// 重复分发，由技能侧账号级去重兜底。计数只能进错误文本——见 run() 里关于结果 payload 的说明。
func TestLikeSkillWorkflowMarkError(t *testing.T) {
	env, marks := newLikeTestEnv(t,
		[]db.RawItem{{Source: db.PlatformX, ExternalID: "x_1", URL: likeURL1}},
		nil,
		map[string]outcome{likeURL1: {ok: true, tweetID: "1", success: 1}},
		errors.New("mark failed"),
	)

	result, err := env.run(LikeSkillInput{})
	if err == nil || !strings.Contains(err.Error(), "mark failed") {
		t.Fatalf("workflow err = %v, want 含 mark failed", err)
	}
	if !strings.Contains(err.Error(), "mark 1 liked tweet(s) failed") {
		t.Fatalf("workflow err = %v, want 带出成功计数（点了 1 条没标记成）", err)
	}
	// 结果 payload 取不回来：Temporal 的 workflow 以 activity 错误终止时，SDK 在结果里
	// 只回一个空对象，半成功计数靠上面那句包装后的错误文本带给调用方。
	if result.Total != 0 {
		t.Fatalf("result = %+v, want 空结果（activity 错误终止时结果 payload 不可用）", result)
	}
	// 回写 activity 按 dbCtx 的有界重试跑了 MaximumAttempts 次（都失败）：断言「尝试过且
	// 只带成功项」，不钉次数——次数是重试策略的表达，归编排层（dbCtx）负责。
	if marks.count() == 0 || marks.joined() != strings.TrimSuffix(strings.Repeat("1,", marks.count()), ",") {
		t.Fatalf("回写 = %q (calls=%d), want 只带成功项 1", marks.joined(), marks.count())
	}
}
