package main

import (
	"log"
	"os"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// driver_worktree_gc.go：进化 worktree 清理（已合并任务回收）与工作项状态回流。
// 自 driver_merge.go 拆出（批次 F，纯机械搬迁，行为无变更）。

func (d *driver) cleanupWorktree(taskNo, trigger string) {
	m := d.wtManager()
	dir := m.Dir(taskNo)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return // 目录已不存在（幂等）
		}
		// 非 ENOENT（权限等）→ 不静默吞：告警 + audit，避免留下无法排查的漏清
		log.Printf("WARN: stat worktree %s: %v", dir, err)
		d.store.Log(d.ctx(), "evolve-driver", "worktree_clean_failed", taskNo, map[string]any{
			"dir": dir, "branch": m.Branch(taskNo), "trigger": trigger, "error": "stat: " + err.Error(),
		})
		return
	}
	// 保守红线（与 Manager.Scan 同口径）：主/子仓库 evolve 分支未合并进各自 base → 绝不清理。
	// 失败态（empty-diff/打回/终止）的目录保留 = 「续接执行」的基础；有未提交改动同样跳过。
	if !m.BranchMerged(m.Branch(taskNo), m.Base) {
		log.Printf("evolve %s: 分支未合并进 %s → 跳过清理（%s）", taskNo, m.Base, dir)
		d.store.Log(d.ctx(), "evolve-driver", "worktree_clean_skipped", taskNo, map[string]any{
			"dir": dir, "branch": m.Branch(taskNo), "trigger": trigger, "reason": "branch not merged",
		})
		return
	}
	// 保守红线：分支已合并 ≠ 工作区干净——有未提交改动（cancelled/打回任务的未提交 diff）
	// 绝不清理。merge-success 路径刚合完工作区已提交，不会命中。
	if m.HasUncommitted(taskNo) {
		log.Printf("evolve %s: worktree 有未提交改动 → 跳过清理（%s）", taskNo, dir)
		d.store.Log(d.ctx(), "evolve-driver", "worktree_clean_skipped", taskNo, map[string]any{
			"dir": dir, "branch": m.Branch(taskNo), "trigger": trigger, "reason": "uncommitted changes",
		})
		return
	}
	if err := m.Remove(taskNo); err != nil {
		log.Printf("WARN: clean worktree %s (%s): %v", taskNo, trigger, err)
		d.store.Log(d.ctx(), "evolve-driver", "worktree_clean_failed", taskNo, map[string]any{
			"dir": dir, "branch": m.Branch(taskNo), "trigger": trigger, "error": err.Error(),
		})
		return
	}
	d.store.Log(d.ctx(), "evolve-driver", "worktree_cleaned", taskNo, map[string]any{
		"dir": dir, "branch": m.Branch(taskNo), "trigger": trigger,
	})
	log.Printf("evolve %s: worktree cleaned (%s, %s)", taskNo, trigger, dir)
}

// cleanupMergedWorktree 重跑前的孤儿目录清理：若 evolve 分支已合并进 base（上次成功收尾在
// merge-success 清理前崩溃）且目录存在 → 删除并返回 true（调用方重建干净 worktree）。
// 未合并/不存在 → false（不动）。子仓库嵌套 worktree 随主 Remove 一并清理。
func (d *driver) cleanupMergedWorktree(task *db.EvolveTask) bool {
	m := d.wtManager()
	if !m.BranchMerged(m.Branch(task.TaskNo), m.Base) {
		return false
	}
	if _, err := os.Stat(m.Dir(task.TaskNo)); err != nil {
		return false
	}
	d.cleanupWorktree(task.TaskNo, "rerun-merged")
	// 清理可能因保守红线（脏工作区）被跳过 → 只有目录真的没了才算重建
	if _, err := os.Stat(m.Dir(task.TaskNo)); err != nil {
		log.Printf("evolve %s: 检测到已合并的孤儿 worktree，重跑前重建干净目录", task.TaskNo)
		return true
	}
	return false
}

// cleanMergedWorktrees 兜底周期扫描（独立 ticker，EVOLVE_CLEAN_SCAN_SEC，默认 30 分钟）：
// 扫 cache/evolve/，对「分支已合并进 main 且不在进行中且无未提交改动」的目录执行清理。
// 安全红线：active 集合（运行态/审批中/已上线未合并）内的任务绝不被误删；
// 未合并分支 / 有未提交改动的 worktree 一律保留；active 查询失败 → 跳过本轮（fail-safe）。
func (d *driver) cleanMergedWorktrees() {
	if d.cfg.CleanScanSec <= 0 {
		return
	}
	// 红线：清理保护名单 = 运行态（open/coding/…）+ released 未合并 + **failed/cancelled**。
	// 只有成功（merge-success）才删除 worktree；失败任务目录保留，供看板「续接执行」复用。
	active, err := d.activeTaskNos()
	if err != nil {
		log.Printf("WARN: cleanMergedWorktrees: %v（跳过本轮扫描，不用残缺保护名单）", err)
		return
	}
	m := d.wtManager()
	stale, err := m.Scan(active)
	if err != nil {
		log.Printf("WARN: scan worktrees: %v", err)
		return
	}
	for _, taskNo := range stale {
		d.cleanupWorktree(taskNo, "periodic-scan")
	}
}

// syncWorkItemOnRun 任务开跑时对齐看板侧状态（幂等）：
//   - 关联工作项（evidence.work_item_no）不在运行态（如 cancelled 回落的 rejected）→ 拉回 in_progress
//     （否则看板看不到在跑的任务；且工作项终结态时用户再 hand-off 会重复建任务）；
//   - 关联看板卡的 work_item_no 指回本次运行的工作项（卡状态/列由 work-dispatch 镜像轮同步：
//     in_progress → 卡 in_progress + In Progress 列）。
//
// 只作用本任务及其关联卡，不扫全表。
func (d *driver) syncWorkItemOnRun(task *db.EvolveTask) {
	if task.Source != "work_item" {
		return
	}
	ev := parseEvidence(task.Evidence)
	itemNo, _ := ev["work_item_no"].(string)
	if itemNo == "" {
		return
	}
	var item db.WorkItem
	if err := d.db.Where("no = ?", itemNo).First(&item).Error; err != nil {
		return // 工作项已清理 → 不动（不阻断任务）
	}
	if item.Status != "in_progress" {
		if err := d.db.Model(&db.WorkItem{}).Where("no = ?", itemNo).
			Updates(map[string]any{"status": "in_progress", "updated_at": time.Now()}).Error; err != nil {
			log.Printf("WARN: revive work item %s: %v", itemNo, err)
		} else {
			d.store.Log(d.ctx(), "evolve-driver", "work_item_revived", task.TaskNo, map[string]any{
				"work_item_no": itemNo, "from": item.Status, "to": "in_progress",
			})
			log.Printf("evolve %s: work item %s revived (%s → in_progress)", task.TaskNo, itemNo, item.Status)
		}
	}
	if item.CardID == nil || *item.CardID == 0 {
		return
	}
	var card db.BoardCard
	if err := d.db.First(&card, *item.CardID).Error; err != nil {
		return
	}
	if card.WorkItemNo == itemNo {
		return
	}
	if err := d.db.Model(&db.BoardCard{}).Where("id = ?", card.ID).
		Updates(map[string]any{"work_item_no": itemNo, "updated_at": time.Now()}).Error; err != nil {
		log.Printf("WARN: relink card %d → %s: %v", card.ID, itemNo, err)
		return
	}
	d.store.Log(d.ctx(), "evolve-driver", "card_relinked", task.TaskNo, map[string]any{
		"card_id": card.ID, "work_item_no": itemNo, "prev": card.WorkItemNo,
	})
	log.Printf("evolve %s: card %d relinked → work item %s (prev=%s)", task.TaskNo, card.ID, itemNo, card.WorkItemNo)
}
