package workitem

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
)

// 启动前依赖检查（2026-10-29）：
// 任务之间可能有前置关系（B 依赖 A），A 没完成就启动 B 会白做/失败。
// 在「开始执行」前用 LLM 看一遍看板，判断目标卡是否依赖其他**未完成**任务。
//
// 设计取舍：
//   - **fail-open**：LLM 未配置 / 超时 / 输出不可解析 → 放行（模型故障不能把看板卡死）；
//   - 只信「未完成」卡：Done 的卡不构成阻塞；
//   - 只信看板里真实存在的 id：模型编造的 id 一律忽略；若最终没有有效阻塞项 → 放行。

// DependencyBlocker 是一条阻塞依赖（未完成/未合入主干的前置卡）。
// 类型别名到 db.CardDependency：看板读取时也用它表达阻塞，避免两个同构结构分裂。
type DependencyBlocker = db.CardDependency

const (
	dependencyMaxCards  = 40
	dependencyDescRunes = 200
	dependencyTimeout   = 30 * time.Second
)

const dependencySystemPrompt = "你是看板任务调度助手。判断「目标任务」现在是否可以开始执行。\n" +
	"规则：\n" +
	"1) 只有当某个**未完成**任务确实是目标任务的硬性前置（必须先完成它，目标才能做/才不会白做）时才判定阻塞；\n" +
	"2) 仅凭标题与描述判断，不要臆测；不确定就不要阻塞；\n" +
	"3) 已完成（Done）的任务不算阻塞；\n" +
	"4) 只能引用输入里给出的任务 id。\n" +
	"只输出一个 JSON 对象（不要 markdown、不要解释）：\n" +
	`{"blocked": true|false, "blocked_by": [任务id...], "reason": "中文一句话"}`

// CheckDependencies 用 LLM 判断 target 能否立即开始：返回未完成的依赖卡（空 = 可执行）。
// fail-open：llmClient 为 nil / 调用失败 / 输出不可解析 → (nil, "")。
//
// 保留旧签名（不知库里合并状态）：完成口径退回「卡片是否在 Done 列」。
// 生产调用点请用 CheckDependenciesWithStore（显式依赖优先 + 合并门禁）。
func CheckDependencies(ctx context.Context, llmClient *llm.Client, cards []db.BoardCard, target db.BoardCard) ([]DependencyBlocker, string) {
	return checkDependenciesLLM(ctx, llmClient, cards, target, nil)
}

// checkDependenciesLLM LLM 推断路径。merged 为「卡片已完成（已合入主干）」映射；
// 为 nil 时退回看列（isCardDone）——旧测试与无库场景用。
func checkDependenciesLLM(ctx context.Context, llmClient *llm.Client, cards []db.BoardCard, target db.BoardCard, merged map[uint]bool) ([]DependencyBlocker, string) {
	if llmClient == nil {
		return nil, ""
	}
	done := func(c db.BoardCard) bool {
		if merged != nil {
			return merged[c.ID]
		}
		return isCardDone(c)
	}
	byID := make(map[uint]db.BoardCard, len(cards))
	unfinished := make([]db.BoardCard, 0, len(cards))
	for _, c := range cards {
		if c.ID == target.ID {
			continue
		}
		byID[c.ID] = c
		if done(c) {
			continue
		}
		unfinished = append(unfinished, c)
	}
	if len(unfinished) == 0 {
		return nil, ""
	}

	ctxL, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()
	raw, err := llmClient.ChatCompletion(ctxL, dependencySystemPrompt, dependencyUserPrompt(unfinished, target))
	if err != nil {
		return nil, "" // fail-open
	}
	verdict, ok := parseDependencyVerdict(raw)
	if !ok || !verdict.Blocked {
		return nil, ""
	}
	blockers := make([]DependencyBlocker, 0, len(verdict.BlockedBy))
	for _, id := range verdict.BlockedBy {
		c, exists := byID[id]
		if !exists || done(c) {
			continue // 编造的 id / 已合入主干的卡 → 不算阻塞
		}
		blockers = append(blockers, DependencyBlocker{CardID: c.ID, Title: c.Title, Status: c.Status})
	}
	if len(blockers) == 0 {
		return nil, "" // 只有「合法阻塞项」才拦
	}
	reason := strings.TrimSpace(verdict.Reason)
	if reason == "" {
		reason = "存在未完成的前置任务，已阻止启动"
	}
	return blockers, reason
}

// dependencyVerdict 是 LLM 输出结构。
type dependencyVerdict struct {
	Blocked   bool   `json:"blocked"`
	BlockedBy []uint `json:"blocked_by"`
	Reason    string `json:"reason"`
}

// dependencyUserPrompt 组装看板快照（目标任务 + 其他未完成任务）。
func dependencyUserPrompt(unfinished []db.BoardCard, target db.BoardCard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标任务:\nid=%d | 标题: %s\n描述: %s\n目标项目: %s\n\n其他未完成任务:\n",
		target.ID, target.Title, truncateRunes(target.Description, dependencyDescRunes), target.TargetName)
	n := 0
	for _, c := range unfinished {
		if n >= dependencyMaxCards {
			fmt.Fprintf(&b, "…（还有 %d 个未完成任务已省略）\n", len(unfinished)-n)
			break
		}
		fmt.Fprintf(&b, "id=%d | 状态: %s | 标题: %s | 描述: %s\n",
			c.ID, c.Status, c.Title, truncateRunes(c.Description, dependencyDescRunes))
		n++
	}
	return b.String()
}

// parseDependencyVerdict 从 LLM 输出抽取 JSON（容忍代码块围栏/前后杂文）。
func parseDependencyVerdict(raw string) (dependencyVerdict, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end < start {
		return dependencyVerdict{}, false
	}
	var v dependencyVerdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return dependencyVerdict{}, false
	}
	return v, true
}

func isCardDone(c db.BoardCard) bool {
	return strings.EqualFold(strings.TrimSpace(c.Status), "done")
}

func truncateRunes(s string, max int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
