package main

import (
	"fmt"
	"log"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

func (d *driver) setRunning(taskNo string) { d.running.Store(taskNo) }

func (d *driver) runningTaskNo() string {
	if v, ok := d.running.Load().(string); ok {
		return v
	}
	return ""
}

// mergeGateActive 是否需要「上一个任务未合入 → 阻塞新任务」门禁（仅本地合入模式生效；
// pr/off 模式本来就不做本地合入，不能据此阻塞）。
func mergeGateActive(mode string) bool { return mode == "local" || mode == "both" }

// mergeBlockedByUnmerged 是否有「已上线但尚未合入 main」的任务：
// 有则阻塞新任务拾取（串行管线：上一个任务没落到 main 之前不开新单），直到合入成功；
// 已写 pr_error（需人工处理）的不算，避免管线被永久卡死。
func (d *driver) mergeBlockedByUnmerged() (string, bool) {
	if !mergeGateActive(d.cfg.MergeMode) {
		return "", false
	}
	var t db.EvolveTask
	if err := d.db.Where("status = ? AND (merge_commit IS NULL OR merge_commit = '') AND (pr_error IS NULL OR pr_error = '')", "released").
		Order("id").First(&t).Error; err != nil {
		return "", false
	}
	return t.TaskNo, true
}

func (d *driver) tick() {
	// 阻塞门禁：上一个任务还没合入 main → 不拾取新任务（看板上该任务显示为等待合入/阻塞）
	if taskNo, blocked := d.mergeBlockedByUnmerged(); blocked {
		if d.mergeBlockAudited.CompareAndSwap(false, true) {
			log.Printf("evolve: %s 尚未合入 main → 阻塞新任务执行，直到合入成功", taskNo)
			d.store.Log(d.ctx(), "evolve-driver", "queue_blocked_unmerged", taskNo, map[string]any{
				"reason": "released but not merged to main（阻塞新任务拾取）",
			})
		}
		return
	}
	d.mergeBlockAudited.Store(false)

	// 注：发布跟进（maybePublish）与陈旧回收（reapStaleTasks）已拆到独立 ticker 协程（见 main），
	// 不在此处串行——否则 runTask 跑长任务时会把 released → mergeToMain 馔死。
	var task db.EvolveTask
	if err := d.db.Where("status = ?", "open").Order("id").First(&task).Error; err != nil {
		return // 无待处理进化单
	}
	log.Printf("evolve: picked task %s (%s, source=%s)", task.TaskNo, task.TargetName, task.Source)
	d.refreshModelsFromProfile() // S8：active 模型档案 consumers 含 coder/reviewer → 本次执行生效
	d.runTask(&task)
}

// recoverInterruptedTasks 启动恢复：进程重启（重建镜像/OOM/宿主重启）后，上次中断的运行态任务
// 会永久停在 coding/reviewing/sandboxing/testing，锁死并发上限 1 停摆整条队列。
// reapStaleTasks 要等 StaleSec（默认 2h）才回收，期间队列停摆 → 启动时立即重置为 open 重跑。
// 幂等：worktree 保留（ensureWorktree 复用）+ session_id 保留（pi-web 会话续接），coder 从上次中断处继续。
func (d *driver) recoverInterruptedTasks() {
	var tasks []db.EvolveTask
	if err := d.db.Where("status IN ?",
		[]string{"coding", "reviewing", "sandboxing", "testing"}).
		Find(&tasks).Error; err != nil {
		log.Printf("WARN: recover interrupted tasks: %v", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		if err := d.db.Model(t).Update("status", "open").Error; err != nil {
			log.Printf("WARN: reset interrupted task %s: %v", t.TaskNo, err)
			continue
		}
		d.store.Log(d.ctx(), "evolve-driver", "task_recovered", t.TaskNo, map[string]any{
			"prev_status": t.Status, "target": t.TargetName, "session_id": t.SessionID,
		})
		log.Printf("evolve %s: 启动恢复（prev=%s）→ 重置 open 重跑", t.TaskNo, t.Status)
	}
}

// reapStaleTasks 把运行态且超过阈值未更新的进化单重置为 open（进程崩溃自愈，见 tick）。
// 跳当前正在执行的任务（running）：独立协程下它能与 runTask 并发，而长阶段（coder/reviewer）
// 期间不会写库刷新 updated_at，只看时间戳会误伤。
func (d *driver) reapStaleTasks() {
	if d.cfg.StaleSec <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(d.cfg.StaleSec) * time.Second)
	var tasks []db.EvolveTask
	if err := d.db.Where("status IN ? AND updated_at < ?",
		[]string{"coding", "reviewing", "sandboxing", "testing"}, cutoff).
		Find(&tasks).Error; err != nil {
		log.Printf("WARN: reap stale tasks: %v", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		if t.TaskNo == d.runningTaskNo() {
			continue // 本进程正在跑：不是陈旧，是无进度上报
		}
		if err := d.db.Model(t).Update("status", "open").Error; err != nil {
			log.Printf("WARN: reset stale task %s: %v", t.TaskNo, err)
			continue
		}
		d.store.Log(d.ctx(), "evolve-driver", "stale_task_reset", t.TaskNo, map[string]any{
			"stale_sec": d.cfg.StaleSec, "target": t.TargetName,
		})
		log.Printf("evolve %s: stale (>%ds 未更新) → 重置 open 重跑", t.TaskNo, d.cfg.StaleSec)
	}
}

// ---- worktree 清理（合并成功后即时 + 周期扫描兜底，2026-10-26）----

// wtManager 返回 worktree 管理器（主仓库 worktree 生命周期：合并查询 / 删除 / 扫描）。
func (d *driver) wtManager() *worktreeManager {
	return newWorktreeManager(d.cfg.RepoDir, d.cfg.RepoDir+"/"+d.cfg.EvolveCacheDir, d.cfg.BaseBranch)
}

// activeTaskNos 查「进行中」的进化单号集合（绝不可清理的 worktree）：
// 运行态（open/coding/reviewing/sandboxing/testing）+ 审批中（approving）+ 已上线未合并
// （released 且 merge_commit 为空：mergeToMain 还要用 worktree 落地）。
// 任一查询失败 → 返回 error（fail-safe：调用方跳过本轮扫描，绝不用残缺保护名单去删目录）。
func (d *driver) activeTaskNos() (map[string]bool, error) {
	active := map[string]bool{}
	var tasks []db.EvolveTask
	if err := d.db.Where("status IN ?", []string{"open", "coding", "reviewing", "sandboxing", "testing", "approving"}).
		Find(&tasks).Error; err != nil {
		return nil, fmt.Errorf("scan active tasks: %w", err)
	}
	for _, t := range tasks {
		active[t.TaskNo] = true
	}
	var unmerged []db.EvolveTask
	if err := d.db.Where("status = ? AND (merge_commit IS NULL OR merge_commit = '')", "released").Find(&unmerged).Error; err != nil {
		return nil, fmt.Errorf("scan unmerged released tasks: %w", err)
	}
	for _, t := range unmerged {
		active[t.TaskNo] = true
	}
	var failed []db.EvolveTask
	if err := d.db.Where("status IN ?", []string{"failed", "cancelled"}).Find(&failed).Error; err != nil {
		return nil, fmt.Errorf("scan failed tasks: %w", err)
	}
	for _, t := range failed {
		active[t.TaskNo] = true
	}
	return active, nil
}

// cleanupWorktree 删除单个任务的 worktree（含 git 注册 + 嵌套子仓库 worktree + 目录），
// 并写 audit_logs（目录路径、分支名、触发原因）。失败只告警不阻断（周期扫描下轮再试）。
