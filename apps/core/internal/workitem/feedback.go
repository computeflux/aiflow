package workitem

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
)

// FeedbackResult 是一轮回流的统计。
type FeedbackResult struct {
	Scanned    int
	Shipped    int
	Rejected   int
	Expired    int
	InProgress int
	Errors     []string
}

// releasedLadder 是任务已 released 时把工作项推到 shipped 的合法路径（逐级补齐，不得跳级）。
// 工作项当时可能停在其中任一级（扫描范围为 in_progress/in_review/approved），已走过的级会跳过。
var releasedLadder = []string{StatusInReview, StatusApproved, StatusShipped}

// Feedback 执行一轮回流（§7.7）：扫所有关联了进化单且未完结（in_progress/in_review/approved）
// 的工作项，按 S1 进化单 status 映射并更新工作项状态：
//   - released → shipped（逐级补齐 in_review → approved → shipped；+ 回填该簇 error_logs resolved=true）
//   - approving → in_review
//   - rolled_back → Bug 重新入队重修（强制 P0，§7.7）；需求 → rejected
//   - cancelled → rejected
//   - expired → expired（可人工/自动重新入队）
//   - open/coding/reviewing/sandboxing/testing → 保持 in_progress
func Feedback(ctx context.Context, store *Store, w ScoreWeights, nt notify.Notifier) (*FeedbackResult, error) {
	res := &FeedbackResult{}
	items, err := store.OpenWithEvolveTask(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan open work items: %w", err)
	}
	res.Scanned = len(items)

	for _, item := range items {
		from := item.Status
		var task db.EvolveTask
		if err := store.database.WithContext(ctx).Where("task_no = ?", item.EvolveTaskNo).First(&task).Error; err != nil {
			continue // 进化单不存在（未建/被清理）→ 保持现状
		}
		switch task.Status {
		case "released":
			// 沿合法状态机逐级补齐到 shipped：in_progress → in_review → approved → shipped。
			// 审批常在两个回流周期（5 分钟）之间就完成：任务已 released 而工作项仍停在 in_progress；
			// 若直接 in_progress → shipped 会被状态机拒绝（非法迁移）→ 每轮回流报错、工作项永久卡住
			// （2026-10-25 实测 wi-1792908636-6b61 卡 20+ 轮：审批 06:48 通过、回流未跑到 in_review）。
			cur := item.Status
			// 只补「当前阶段之后」的阶段：in_progress（不在 ladder 内）→ 补全部；
			// in_review → approved → shipped；approved → 只补 shipped。
			// （不能用 cur == to 判断：从 approved 出发时 cur 不等于 in_review，会倒退成非法迁移。）
			start := 0
			for i, stage := range releasedLadder {
				if stage == cur {
					start = i + 1
					break
				}
			}
			stepFailed := false
			for _, to := range releasedLadder[start:] {
				if err := store.UpdateStatus(ctx, item.No, to); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("feedback %s: %v", item.No, err))
					stepFailed = true
					break
				}
				logTransition(ctx, store, &item, cur, to, task.Status)
				cur = to
			}
			if stepFailed {
				continue // 未补齐到 shipped → 下轮再试（不写 shipped 侧效应）
			}
			backfillErrorLogs(ctx, store, &item)
			backfillCard(ctx, store, &item, StatusShipped) // 关联看板卡 → done（§7.6 看板回流）
			res.Shipped++

		case "approving":
			// 审批单已通过（灰度中）→ approved；仍待审批 → in_review。
			// 状态不变时不重复写库/写日志（修复：每轮空迁移 in_review→in_review 刷屏，2026-10-08 实测 19 条）
			to := StatusInReview
			var apr db.Approval
			if task.ApprovalID != nil &&
				store.database.WithContext(ctx).First(&apr, *task.ApprovalID).Error == nil && apr.Status == "approved" {
				to = StatusApproved
			}
			if item.Status != to {
				if err := store.UpdateStatus(ctx, item.No, to); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("feedback %s: %v", item.No, err))
					continue
				}
				logTransition(ctx, store, &item, item.Status, to, task.Status)
			}
			res.InProgress++

		case "rolled_back":
			if item.Kind == KindBug {
				// 回滚的 Bug：修复未生效 → 重新入队重修（强制 P0 插队，§7.7）；
				// 重修次数超限 → rejected（防 修复→上线→回滚 无限循环，根因常在 coder 控制之外；
				// 看板可重新 hand-off 重跑）。
				// 这是系统驱动的重新入队（非用户操作），直接更新，不走用户态状态机。
				to := requeueAtP0(ctx, store, &item, w, nt)
				logTransition(ctx, store, &item, from, to, task.Status)
				if to == StatusRejected {
					backfillCard(ctx, store, &item, StatusRejected) // 关联看板卡 → todo（Backlog，可重新 hand-off）
				}
			} else {
				if err := store.UpdateStatus(ctx, item.No, StatusRejected); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("feedback %s: %v", item.No, err))
					continue
				}
				logTransition(ctx, store, &item, from, StatusRejected, task.Status)
				backfillCard(ctx, store, &item, StatusRejected) // 关联看板卡 → todo（Backlog，可重新 hand-off）
			}
			res.Rejected++

		case "cancelled":
			// 人工已把工作项推到 in_review / approved（看板「完成执行」）→ 不在本轮驳回，
			// 否则刚推进 Review 的卡会被旧进化单的 cancelled 拉回 Backlog。
			if item.Status == StatusInReview || item.Status == StatusApproved {
				res.InProgress++
				continue
			}
			if err := store.UpdateStatus(ctx, item.No, StatusRejected); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("feedback %s: %v", item.No, err))
				continue
			}
			logTransition(ctx, store, &item, from, StatusRejected, task.Status)
			backfillCard(ctx, store, &item, StatusRejected) // 关联看板卡 → todo（Backlog，可重新 hand-off）
			res.Rejected++

		case "failed":
			// 2026-11-03：失败**不打回 Backlog** —— 工作项保持 in_progress，
			// 卡片留在 In Progress，由人工在看板选「忽略审核 / 续接执行 / 终止」。
			// （失败原因写在 evolve_tasks.fail_reason，卡面直接展示）
			res.InProgress++

		case "stopped":
			// 看板「完成执行」：任务已人工停下，工作项由接口推成 in_review。
			// 这里不能再改工作项状态（否则会把刚推上去的 in_review 拉回去）。
			res.InProgress++

		case "expired":
			if err := store.UpdateStatus(ctx, item.No, StatusExpired); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("feedback %s: %v", item.No, err))
				continue
			}
			logTransition(ctx, store, &item, from, StatusExpired, task.Status)
			res.Expired++

		default:
			res.InProgress++ // open/coding/reviewing/sandboxing/testing：保持 in_progress
		}
	}
	return res, nil
}

// backfillCard 工作项终结 → 回填关联看板卡状态（§7.6 看板回流闭环）：
// shipped → done（任务完成）；rejected → todo（Backlog，可重新 hand-off 重跑）。
// 仅当工作项关联了卡（card_id 非空）。写失败不阻断（卡状态是次要留痕，工作项状态与 audit_logs 才是权威）。
func backfillCard(ctx context.Context, store *Store, item *db.WorkItem, toStatus string) {
	if item.CardID == nil || *item.CardID == 0 {
		return
	}
	var cardStatus string
	switch toStatus {
	case StatusShipped:
		cardStatus = "done"
	case StatusRejected:
		cardStatus = "todo"
	default:
		return // 非终结态（in_review/expired 等）不回填卡
	}
	_ = store.database.WithContext(ctx).Model(&db.BoardCard{}).
		Where("id = ?", *item.CardID).
		Updates(map[string]any{"status": cardStatus, "updated_at": time.Now()})
}

// maxRollbackRequeues：回滚 Bug 重修入队次数上限。超限 → rejected：
// 修复反复通过 CI 却灰度失败，自动重修只会空转（根因常在 coder 控制之外：基础设施延迟/依赖），
// 自动重修停止（S3 watchdog 只扫 Temporal 死循环，不覆盖这条环）。
const maxRollbackRequeues = 3

// requeueAtP0 把回滚的 Bug 工作项重新入队并强制 P0（§7.7：下轮插队重修）；
// 重修次数（RollbackCount）超 maxRollbackRequeues → rejected + 告警。返回落到的状态。
func requeueAtP0(ctx context.Context, store *Store, item *db.WorkItem, w ScoreWeights, nt notify.Notifier) string {
	if item.RollbackCount >= maxRollbackRequeues {
		store.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", item.No).Updates(map[string]any{
			"status": StatusRejected, "rollback_count": item.RollbackCount + 1, "updated_at": time.Now(),
		})
		_ = nt.SendAlert(ctx, notify.Alert{
			Title: fmt.Sprintf("工作项 %s 灰度回滚 %d 次，自动重修已停", item.No, item.RollbackCount+1),
			Body:  fmt.Sprintf("target=%s 的修复反复灰度失败（重修上限 %d），自动重修已停，需在看板重新 hand-off。%s", item.TargetName, maxRollbackRequeues, item.Title),
			Level: "critical",
		})
		return StatusRejected
	}
	score := ComputeScore(w, 4, 4, 4, item.Frequency)
	store.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", item.No).Updates(map[string]any{
		"status": StatusQueued, "score": score, "priority": "P0",
		"severity": 4, "urgency": 4, "impact": 4, "rollback_count": item.RollbackCount + 1, "updated_at": time.Now(),
	})
	return StatusQueued
}

// logTransition 写一条 audit_logs（S1 留痕红线：只增不删，无删除 API）。
// 回流是系统驱动的（非用户操作），actor 固定 work-dispatch；detail 记录工作项/进化单/状态迁移。
// 审计写失败不能静默（留痕是验收前提）：打 ERROR 日志，不阻断回流。
func logTransition(ctx context.Context, store *Store, item *db.WorkItem, from, to, taskStatus string) {
	if from == to {
		return // 空迁移不写（否则每轮刷屏：in_review→in_review 等）
	}
	detail := map[string]any{
		"work_item_no":   item.No,
		"kind":           item.Kind,
		"source":         item.Source,
		"evolve_task_no": item.EvolveTaskNo,
		"task_status":    taskStatus,
		"from":           from,
		"to":             to,
	}
	var detailJSON string
	if b, err := json.Marshal(detail); err == nil {
		detailJSON = string(b)
	}
	entry := db.AuditLog{Actor: "work-dispatch", Action: "work_item_feedback", Target: item.No, Detail: detailJSON}
	if err := store.database.WithContext(ctx).Create(&entry).Error; err != nil {
		log.Printf("ERROR: write audit_log (work-dispatch %s -> %s): %v", item.No, to, err)
	}
}

// backfillErrorLogs 上线成功 → 回填该工作项归并的 error_logs（resolved=true，§7.7）。
func backfillErrorLogs(ctx context.Context, store *Store, item *db.WorkItem) {
	if item.Source != SourceErrorLog {
		return
	}
	store.database.WithContext(ctx).Model(&db.ErrorLog{}).
		Where("work_item_no = ?", item.No).
		Updates(map[string]any{"resolved": true, "resolved_at": time.Now()})
}
