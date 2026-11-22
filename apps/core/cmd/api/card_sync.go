package main

import (
	"context"
	"errors"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workitem"
	"gorm.io/gorm"
)

// findCardPendingApproval 找卡当前待审批的审批单（S9 看板直接审批）：
// 卡 → 最新工作项（card_id，不论状态）→ 进化单（evolve_task_no）→ 审批单（approval_id，pending）。
// 以「是否存在 pending 审批单」为唯一判据（不看卡状态，避免卡状态镜像延迟导致按钮不显示）。
// 无工作项 / 无进化单 / 无待审批单 → (nil, nil)。
func findCardPendingApproval(ctx context.Context, d *db.DB, cardID int) (*db.Approval, error) {
	var item db.WorkItem
	err := d.WithContext(ctx).
		Where("card_id = ?", cardID).
		Order("id DESC").First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// card_id 可能为 NULL（旧数据）→ 反向查：卡的 work_item_no → 工作项
		var card db.BoardCard
		if cerr := d.WithContext(ctx).Where("id = ?", cardID).First(&card).Error; cerr != nil {
			return nil, nil
		}
		if card.WorkItemNo == "" {
			return nil, nil
		}
		err = d.WithContext(ctx).Where("no = ?", card.WorkItemNo).First(&item).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	if item.EvolveTaskNo == "" {
		return nil, nil
	}
	var task db.EvolveTask
	if err := d.WithContext(ctx).Where("task_no = ?", item.EvolveTaskNo).First(&task).Error; err != nil {
		return nil, err
	}
	if task.ApprovalID == nil || *task.ApprovalID == 0 {
		return nil, nil
	}
	var ap db.Approval
	if err := d.WithContext(ctx).Where("id = ?", *task.ApprovalID).First(&ap).Error; err != nil {
		return nil, err
	}
	if ap.Status != "pending" {
		return nil, nil
	}
	return &ap, nil
}

// syncCardStatusToWorkItem 把看板卡状态变更翻译为工作项状态机迁移（S9 双向同步，看板 → 工作流）：
//   - done → shipped（人工验收）；ready_for_evolve → queued（入队/重新入队）；
//   - todo → open（收回重开）；in_progress / in_review → 不操作（流水线执行中，用户不能强制跳态）。
//
// S9.8 §7.10 起无 manual_pr 卡态。非法迁移由状态机拒绝（返回错误，调用方 WARN 留痕）；
// 下轮镜像同步会把卡状态重新对齐为工作项状态。
func syncCardStatusToWorkItem(ctx context.Context, store *workitem.Store, workItemNo, cardStatus string) error {
	var to string
	switch cardStatus {
	case "done":
		to = workitem.StatusShipped
	case "ready_for_evolve":
		// 状态机不允许 open → queued（跳过分诊）：未入队项先补 triaged 一步（人工/HITL 确认即分诊通过）
		if it, err := store.ByNo(ctx, workItemNo); err == nil &&
			(it.Status == workitem.StatusOpen || it.Status == workitem.StatusTriaged) {
			return workitem.QueueFromPreTriage(ctx, store, workItemNo, it.Status)
		}
		to = workitem.StatusQueued
	case "todo":
		to = workitem.StatusOpen
	default:
		return nil // in_progress / in_review：流水线执行中，不操作
	}
	return store.UpdateStatus(ctx, workItemNo, to)
}

// syncCardColumnToWorkItem 把"卡被拖到某列"翻译为工作项状态机迁移（2026-10-28 列语义对齐）：
//   - 拖到 Done → shipped（人工验收；仅 approved → shipped 合法，其余由看板界面前置拦下）；
//   - 拖到 Backlog → open（收回候选，等下次 hand-off）；
//   - 拖到 Todo / In Progress → 由调用方的 HandOffCard（+ PickItem）处理，这里不操作；
//   - 拖到 Review → 不操作（流水线自动进入）。
//
// 非法迁移由状态机拒绝（WARN 留痕）；下轮镜像同步会把卡重新对齐到流水线进度所在列。
func syncCardColumnToWorkItem(ctx context.Context, database *db.DB, store *workitem.Store, workItemNo string, listID uint) error {
	var list db.BoardList
	if err := database.DB.WithContext(ctx).Where("id = ?", listID).First(&list).Error; err != nil {
		return nil // 列不存在 → 不操作
	}
	switch list.Name {
	case "Done":
		return store.UpdateStatus(ctx, workItemNo, workitem.StatusShipped)
	case "Backlog":
		return store.UpdateStatus(ctx, workItemNo, workitem.StatusOpen)
	default:
		return nil // Todo / In Progress：hand-off 处理；Review：流水线自动
	}
}

// updateJobStatus 发布任务审批状态变更 + 审计留痕。
func updateJobStatus(database *db.DB, id uint, status string, note string) error {
	updates := map[string]any{"status": status}
	if status == db.StatusApproved {
		now := time.Now()
		updates["approved_at"] = &now
	}
	database.Model(&db.PublishJob{}).Where("id = ?", id).Updates(updates)
	return database.Create(&db.AuditLog{JobID: &id, Action: status, Actor: "admin", Note: note}).Error
}
