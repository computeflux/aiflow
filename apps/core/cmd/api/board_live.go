package main

import (
	"context"
	"strings"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workitem"
)

// applyBlockedBy 读取时计算每张卡的**显式前置依赖**阻塞状态（2026-10-30）：
// 依赖卡未合入主分支 → 卡上带 blocked_by，前端展示「需 #137 xxx 完成后才开始」。
// 只算 card.depends_on（确定性）；LLM 推断不可能每次请求都跑。只改响应，不写库。
func applyBlockedBy(ctx context.Context, database *db.DB, board *db.Board) {
	if board == nil || len(board.Lists) == 0 {
		return
	}
	cards := make([]db.BoardCard, 0, 64)
	for i := range board.Lists {
		cards = append(cards, board.Lists[i].Cards...)
	}
	if len(cards) == 0 {
		return
	}
	blocked := workitem.BlockedCards(ctx, workitem.NewStore(database.DB), cards)
	if len(blocked) == 0 {
		return
	}
	for i := range board.Lists {
		for j := range board.Lists[i].Cards {
			c := &board.Lists[i].Cards[j]
			if b, ok := blocked[c.ID]; ok {
				c.BlockedBy = b
			}
		}
	}
}

// applyLiveCardStatus 读取时实时投影卡状态与所在列（2026-10-29）：
// 以关联 evolve_task 的状态为准（driver 在 coding/approving/released 等节点立即写库），
// 无任务时回退到工作项状态。**只改本次响应（内存），不写库**——
// 落库仍由 work-dispatch 的 5min 镜像轮负责（保证持久一致）。
//
// 目的：看板轮询时能秒级看到「执行中 → 审核中 → 完成」，而不是等 5 分钟镜像轮。
func applyLiveCardStatus(ctx context.Context, database *db.DB, board *db.Board) {
	if board == nil || len(board.Lists) == 0 {
		return
	}
	// 1. 收集关联工作项号（"card-<id>" 是分诊卡关联，不是工作项）
	nos := make([]string, 0, 64)
	for i := range board.Lists {
		for j := range board.Lists[i].Cards {
			no := board.Lists[i].Cards[j].WorkItemNo
			if no != "" && !workitem.IsCardRef(no) {
				nos = append(nos, no)
			}
		}
	}
	if len(nos) == 0 {
		return
	}
	var items []db.WorkItem
	if err := database.WithContext(ctx).Where("no IN ?", nos).Find(&items).Error; err != nil || len(items) == 0 {
		return
	}
	byNo := make(map[string]db.WorkItem, len(items))
	taskNos := make([]string, 0, len(items))
	for _, it := range items {
		byNo[it.No] = it
		if it.EvolveTaskNo != "" {
			taskNos = append(taskNos, it.EvolveTaskNo)
		}
	}
	taskByNo := make(map[string]db.EvolveTask, len(taskNos))
	if len(taskNos) > 0 {
		var tasks []db.EvolveTask
		if err := database.WithContext(ctx).Where("task_no IN ?", taskNos).Find(&tasks).Error; err == nil {
			for _, t := range tasks {
				taskByNo[t.TaskNo] = t
			}
		}
	}

	// 2. 重新分配卡片（仅内存）：按投影后的列名放回对应列表；目标列不存在 → 留在原列
	type entry struct {
		card db.BoardCard
		orig *db.BoardList
	}
	entries := make([]entry, 0, len(nos))
	listsByName := make(map[string]*db.BoardList, len(board.Lists))
	for i := range board.Lists {
		l := &board.Lists[i]
		listsByName[l.Name] = l
		for _, c := range l.Cards {
			entries = append(entries, entry{card: c, orig: l})
		}
		l.Cards = nil
	}
	for _, e := range entries {
		c := e.card
		if it, ok := byNo[c.WorkItemNo]; ok {
			st := effectiveItemStatus(it, taskByNo[it.EvolveTaskNo])
			c.Status = workitem.CardStatusFromItem(st)
			// 投影运行态（不落库）：失败/跳过审核标记让卡面直接显示原因与按钮
			if t, ok := taskByNo[it.EvolveTaskNo]; ok {
				c.EvolveStatus = t.Status
				c.FailReason = t.FailReason
				c.MergeState = t.MergeState
			}
			if target, ok := listsByName[workitem.ColumnFromItem(st)]; ok {
				target.Cards = append(target.Cards, c)
				continue
			}
		}
		e.orig.Cards = append(e.orig.Cards, c)
	}
}

// effectiveItemStatus 把 evolve_task 状态折算成工作项状态（与 workitem.Feedback 同口径）：
//
//	coding/reviewing/sandboxing/testing → in_progress（In Progress）
//	approving                          → in_review（Review）
//	released                           → shipped（Done，**前提是有 merge_commit**）
//	cancelled / rolled_back            → rejected（Backlog）
//	expired                            → expired（Backlog）
//
// **红线（2026-11-03）：代码没合并到 main 就不能显示 Done** ——
// 算出来的结果若是 shipped 但 merge_commit 为空，一律降为 in_review（停 Review）。
func effectiveItemStatus(item db.WorkItem, task db.EvolveTask) string {
	st := effectiveItemStatusRaw(item, task)
	if st == workitem.StatusShipped && strings.TrimSpace(task.MergeCommit) == "" {
		return workitem.StatusInReview
	}
	return st
}

func effectiveItemStatusRaw(item db.WorkItem, task db.EvolveTask) string {
	if task.TaskNo == "" {
		return item.Status
	}
	switch task.Status {
	case "approving":
		return workitem.StatusInReview
	case "released":
		// 已审批发布，但**未合入主分支**（合并冲突 / 合入暂停 / PR 待人工合并）
		// → 停在 Review：主干上没有这段代码，不能显示成 Done，更不能让下游卡误以为可开始。
		if strings.TrimSpace(task.MergeCommit) == "" {
			return workitem.StatusInReview
		}
		return workitem.StatusShipped
	case "cancelled", "rolled_back":
		// 人工已经把工作项推进到 in_review / approved（看板「完成执行」）时，
		// 不能被旧进化单的 cancelled 拉回 Backlog。
		// 注：shipped 不在此列 —— 没合并就不能 Done（由上层红线拦），
		// 这里返回 shipped 后会被降回 in_review。
		switch item.Status {
		case workitem.StatusInReview, workitem.StatusApproved, workitem.StatusShipped:
			return item.Status
		}
		return workitem.StatusRejected
	case "failed":
		// 2026-11-03：失败不打回 Backlog —— 卡片留在 In Progress，等人工选
		// 「续接执行 / 完成执行 / 终止」。
		return workitem.StatusInProgress
	case "stopped":
		// 看板「完成执行」人工停掉的任务：以工作项状态为准（通常已是 in_review → Review 列）。
		return item.Status
	case "expired":
		return workitem.StatusExpired
	case "open", "coding", "reviewing", "sandboxing", "testing":
		return workitem.StatusInProgress
	default:
		return item.Status
	}
}
