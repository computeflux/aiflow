package workitem

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
)

// 显式前置依赖（2026-10-30）
//
// 背景：原有依赖检查是「LLM 看标题/描述猜」+ 只看卡片是否在 Done 列。两个问题：
//  1. 推断不稳定，同一张卡有时拦有时放；
//  2. 「Done」≠「已合入主分支」——任务 released 但合并被暂停/回滚时，
//     下游卡照样会被放行，做出来的东西建在没进主干的代码上。
//
// 现在支持在卡片上显式写前置卡（`depends_on`，如 `[137]`）：
//   - 判定是确定性的（不再问模型）；
//   - 门禁条件是前置卡的改动**已合入主分支**（evolve_task.released 且 merge_commit 非空）；
//   - 未合入时卡片展示「需 #137 xxx 完成后才开始」。
//
// 未填 `depends_on` 的卡仍走原来的 LLM 推断（但「完成」口径同步升级为「已合并」）。

// ParseDependsOn 解析 card.DependsOn：容忍 JSON 数组 / 逗号 / 空格 / 中文逗号 / # 前缀。
func ParseDependsOn(s string) []uint {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" || s == "null" {
		return nil
	}
	var ids []uint
	if strings.HasPrefix(s, "[") {
		if err := json.Unmarshal([]byte(s), &ids); err == nil {
			return dedupIDs(ids)
		}
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ',', ' ', '\t', '\n', ';', '，', '；':
			return true
		}
		return false
	}) {
		f = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(f), "#"))
		if f == "" {
			continue
		}
		if n, err := strconv.ParseUint(f, 10, 64); err == nil {
			ids = append(ids, uint(n))
		}
	}
	return dedupIDs(ids)
}

func dedupIDs(ids []uint) []uint {
	seen := make(map[uint]bool, len(ids))
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// MergedCardSet 批量判定「卡片是否已完成（改动已合入主分支）」。
//
// 口径：
//   - 关联了工作项 + 进化单：evolution_task.status == "released" **且** merge_commit 非空；
//   - 没有工作项/进化单（手工卡）：退回看列（status == done）。
//
// 为什么不用 `status == done`：Done 列只代表工作项 shipped（released），
// 合入被暂停（paused_base_dirty）、回滚、或 PR 模式下还没人工合并时，代码并不在主干上。
func MergedCardSet(ctx context.Context, store *Store, cards []db.BoardCard) map[uint]bool {
	out := make(map[uint]bool, len(cards))
	if store == nil {
		for _, c := range cards {
			out[c.ID] = isCardDone(c)
		}
		return out
	}

	// 1) 卡片 → 工作项号
	nos := make([]string, 0, len(cards))
	cardNo := make(map[uint]string, len(cards))
	for _, c := range cards {
		no := strings.TrimSpace(c.WorkItemNo)
		if no != "" && !IsCardRef(no) {
			cardNo[c.ID] = no
			nos = append(nos, no)
		}
	}

	// 2) 工作项号 → 进化单号
	taskOfItem := map[string]string{}
	if len(nos) > 0 {
		var items []db.WorkItem
		if err := store.database.WithContext(ctx).Where("no IN ?", nos).Find(&items).Error; err == nil {
			for _, it := range items {
				if it.EvolveTaskNo != "" {
					taskOfItem[it.No] = it.EvolveTaskNo
				}
			}
		}
	}

	// 3) 进化单号 → 是否已合入主干
	taskNos := make([]string, 0, len(taskOfItem))
	for _, t := range taskOfItem {
		taskNos = append(taskNos, t)
	}
	mergedTask := map[string]bool{}
	if len(taskNos) > 0 {
		var tasks []db.EvolveTask
		if err := store.database.WithContext(ctx).Where("task_no IN ?", taskNos).Find(&tasks).Error; err == nil {
			for _, t := range tasks {
				mergedTask[t.TaskNo] = t.Status == "released" && strings.TrimSpace(t.MergeCommit) != ""
			}
		}
	}

	for _, c := range cards {
		no := cardNo[c.ID]
		if no == "" {
			out[c.ID] = isCardDone(c) // 手工卡：没有流水线，只看列
			continue
		}
		taskNo := taskOfItem[no]
		if taskNo == "" {
			out[c.ID] = isCardDone(c) // 还没进执行链：同理
			continue
		}
		out[c.ID] = mergedTask[taskNo]
	}
	return out
}

// explicitBlockers 按 card.DependsOn 做确定性判定：返回仍未合入主分支的前置卡。
func explicitBlockers(card db.BoardCard, byID map[uint]db.BoardCard, merged map[uint]bool) ([]db.CardDependency, string) {
	ids := ParseDependsOn(card.DependsOn)
	if len(ids) == 0 {
		return nil, ""
	}
	blockers := make([]db.CardDependency, 0, len(ids))
	for _, id := range ids {
		dep, ok := byID[id]
		if merged[id] && ok {
			continue
		}
		d := db.CardDependency{CardID: id, Merged: merged[id]}
		if ok {
			d.Title = dep.Title
			d.Status = dep.Status
		}
		blockers = append(blockers, d)
	}
	if len(blockers) == 0 {
		return nil, ""
	}
	return blockers, dependencyReason(blockers)
}

// dependencyReason 生成人话原因：「需 #137 xxx 完成后才开始」。
func dependencyReason(blockers []db.CardDependency) string {
	parts := make([]string, 0, len(blockers))
	for _, b := range blockers {
		t := strings.TrimSpace(b.Title)
		if t != "" {
			r := []rune(t)
			if len(r) > 24 {
				t = string(r[:24]) + "…"
			}
			parts = append(parts, fmt.Sprintf("#%d %s", b.CardID, t))
		} else {
			parts = append(parts, fmt.Sprintf("#%d", b.CardID))
		}
	}
	return "需 " + strings.Join(parts, "、") + " 合入主分支后才能开始"
}

// CheckDependenciesWithStore 启动前依赖判定（显式优先，LLM 兜底）：
//   - card.DependsOn 非空 → **只**按显式依赖判定（确定性，不调模型）；
//   - 否则 → 走 LLM 推断，但「完成」口径是「已合入主分支」。
//
// 返回空 blockers = 可执行。
func CheckDependenciesWithStore(ctx context.Context, store *Store, llmClient *llm.Client, cards []db.BoardCard, target db.BoardCard) ([]db.CardDependency, string) {
	byID := make(map[uint]db.BoardCard, len(cards))
	for _, c := range cards {
		if c.ID != target.ID {
			byID[c.ID] = c
		}
	}
	merged := MergedCardSet(ctx, store, cards)

	if ids := ParseDependsOn(target.DependsOn); len(ids) > 0 {
		return explicitBlockers(target, byID, merged)
	}
	return checkDependenciesLLM(ctx, llmClient, cards, target, merged)
}

// BlockedCards 计算看板上每张卡的显式阻塞项（供 GET /boards/:id 展示「需 #xxx 完成后才开始」）。
// 只做显式依赖：LLM 推断每次请求都跑不起，也不可能稳定展示。
func BlockedCards(ctx context.Context, store *Store, cards []db.BoardCard) map[uint][]db.CardDependency {
	byID := make(map[uint]db.BoardCard, len(cards))
	for _, c := range cards {
		byID[c.ID] = c
	}
	var merged map[uint]bool
	out := map[uint][]db.CardDependency{}
	for _, c := range cards {
		if len(ParseDependsOn(c.DependsOn)) == 0 {
			continue
		}
		if merged == nil {
			merged = MergedCardSet(ctx, store, cards)
		}
		if blockers, _ := explicitBlockers(c, byID, merged); len(blockers) > 0 {
			out[c.ID] = blockers
		}
	}
	return out
}
