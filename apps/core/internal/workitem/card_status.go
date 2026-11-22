package workitem

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// card_status.go：工作项 → 看板卡的状态/列镜像同步（S9 §7.6 反向同步，卡随流水线移动）。
// 自 card_sync.go 拆出（批次 F，纯机械搬迁，行为无变更）。

// loadEvolveTask 取进化单（不存在/无编号 → nil）。
func loadEvolveTask(ctx context.Context, store *Store, taskNo string) *db.EvolveTask {
	if strings.TrimSpace(taskNo) == "" {
		return nil
	}
	var t db.EvolveTask
	if err := store.database.WithContext(ctx).Where("task_no = ?", taskNo).First(&t).Error; err != nil {
		return nil
	}
	return &t
}

// CardLane 卡片应处的（状态, 列）。
//
// 关键口径（2026-10-31，2026-11-03 收紧）：shipped 只代表「已审批/已验收」，**不代表代码进了主干**。
// 红线：**没有合并提交就不能进 Done**。
//   - 进化单 released 但 merge_commit 为空（合并冲突 / 合入暂停 / PR 待人工合并）
//   - 或人工「完成执行」后直接验收（进化单是 cancelled/stopped，根本没有合并动作）
//
// 这两种情况一律停在 **Review**，等真合入后再自动进 Done。
// 否则看板显示「已完成」而主干上没有代码，下游卡还会误放行。
func CardLane(item db.WorkItem, task *db.EvolveTask) (status, column string) {
	if item.Status == StatusShipped {
		if task == nil || strings.TrimSpace(task.MergeCommit) == "" {
			return "in_review", "Review"
		}
	}
	return CardStatusFromItem(item.Status), ColumnFromItem(item.Status)
}

// CardStatusSyncResult 是一轮"工作项 → 看板卡"状态镜像的统计（§7.6 看板状态双向同步）。
type CardStatusSyncResult struct {
	Scanned int
	Updated int // 状态变化的卡数
	Moved   int // 列位置变化的卡数（看板为核心：卡随流水线在列间移动）
	Errors  []string
}

// SyncCardStatus 把所有关联了工作项（work_item_no 非空）的看板卡状态镜像为工作项状态（§7.6）。
// 工作项状态是权威，卡状态是实时镜像：中间态（排队中/执行中/审核中）也每轮反映，
// 终结态（shipped→done、rejected/expired→todo）不再依赖单次回填。
// 仅在状态不一致时写库（幂等）；不写 audit_logs（卡状态是派生镜像，工作项迁移已有留痕）。
func SyncCardStatus(ctx context.Context, store *Store) (*CardStatusSyncResult, error) {
	res := &CardStatusSyncResult{}
	var cards []db.BoardCard
	if err := store.database.WithContext(ctx).
		Where("work_item_no != '' AND work_item_no IS NOT NULL").
		Find(&cards).Error; err != nil {
		return nil, fmt.Errorf("scan linked board cards: %w", err)
	}
	res.Scanned = len(cards)

	// 预取全部相关进化单：定列需要它们的合并状态（released 但 merge_commit 空 → 停在 Review）
	items := make([]db.WorkItem, 0, len(cards))
	if err := store.database.WithContext(ctx).
		Where("no IN ?", cardWorkItemNos(cards)).Find(&items).Error; err != nil {
		return nil, fmt.Errorf("scan work items: %w", err)
	}
	itemByNo := make(map[string]db.WorkItem, len(items))
	taskNos := make([]string, 0, len(items))
	for _, it := range items {
		itemByNo[it.No] = it
		if strings.TrimSpace(it.EvolveTaskNo) != "" {
			taskNos = append(taskNos, it.EvolveTaskNo)
		}
	}
	taskByNo := map[string]*db.EvolveTask{}
	if len(taskNos) > 0 {
		var tasks []db.EvolveTask
		if err := store.database.WithContext(ctx).Where("task_no IN ?", taskNos).Find(&tasks).Error; err == nil {
			for i := range tasks {
				taskByNo[tasks[i].TaskNo] = &tasks[i]
			}
		}
	}

	for _, c := range cards {
		item, ok := itemByNo[c.WorkItemNo]
		if !ok {
			continue // 工作项不存在（已清理）→ 卡保持现状
		}
		to, column := CardLane(item, taskByNo[item.EvolveTaskNo])
		updates := map[string]any{"updated_at": time.Now()}
		if to != c.Status {
			updates["status"] = to
		}
		// 看板为核心（§7.6）：卡的位置（列）= 流水线进度的实时镜像，卡随任务在列间移动
		listID, err := moveCardToColumn(ctx, store, c, column)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("card %d move: %v", c.ID, err))
			continue
		}
		if listID != 0 && listID != c.ListID {
			updates["list_id"] = listID
			res.Moved++
		}
		if len(updates) == 1 {
			continue // 状态与列都不变（幂等）
		}
		if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).
			Where("id = ?", c.ID).Updates(updates).Error; err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("card %d mirror: %v", c.ID, err))
			continue
		}
		if to != c.Status {
			res.Updated++
		}
	}
	return res, nil
}
