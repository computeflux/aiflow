package workitem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// 失败后的两种重跑方式（看板按钮，2026-10-18）：
//
//	RerunModeResume  续接执行：复用**同一个进化单**——同一 worktree（上次未提交的代码改动保留）
//	                 + 同一 coder 会话（对话不重发任务描述）+ 已积累的 diff，直接置 open 让
//	                 driver 继续跑。适合环境性失败（LLM 抖动/超时/driver 重启/审核打回）。
//	RerunModeRestart 重新执行：建**新工作项 + 新进化单**——worktree 从 main 干净新建、**全新会话**，
//	                 真从零开始（新工作项不携 evolve_task_no → 不继承 session）。
const (
	RerunModeResume  = "resume"
	RerunModeRestart = "restart"
)

// RerunResult 重跑结果（API 回给看板）。
type RerunResult struct {
	Mode         string `json:"mode"`
	WorkItemNo   string `json:"work_item_no"`
	EvolveTaskNo string `json:"evolve_task_no"`
	TaskStatus   string `json:"task_status"`
	Note         string `json:"note,omitempty"`
}

// RerunCard 按 mode 重跑一张卡（resume 续接 / restart 重新执行）。
// evolveDir 是进化 worktree 根目录（<repo>/cache/evolve），repoRoot 是仓库根：
// resume 用它俩判断上轮目录是否还在、分支是否还在。
func RerunCard(ctx context.Context, store *Store, w ScoreWeights, c *db.BoardCard, mode, evolveDir, repoRoot string) (*RerunResult, error) {
	switch mode {
	case RerunModeResume:
		return resumeCard(ctx, store, c, evolveDir, repoRoot)
	case RerunModeRestart, "":
		return restartCard(ctx, store, w, c)
	default:
		return nil, fmt.Errorf("unknown rerun mode: %s", mode)
	}
}

// latestCardItem 卡最近一次尝试（工作项）：优先 card_id，兜底卡的 work_item_no（旧数据 card_id 为空）。
func latestCardItem(ctx context.Context, store *Store, c *db.BoardCard) (*db.WorkItem, error) {
	var items []db.WorkItem
	if err := store.database.WithContext(ctx).Where("card_id = ?", c.ID).Order("id DESC").Limit(1).Find(&items).Error; err != nil {
		return nil, err
	}
	if len(items) == 0 && c.WorkItemNo != "" && !IsCardRef(c.WorkItemNo) {
		if err := store.database.WithContext(ctx).Where("no = ?", c.WorkItemNo).Order("id DESC").Limit(1).Find(&items).Error; err != nil {
			return nil, err
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &items[0], nil
}

// latestItemWithTask 卡上"最近一个进过执行链"的工作项（evolve_task_no 非空）。
// 续接执行用：卡最新那一项可能只是**空壳**（hand-off 建了新项但即时领取竞争失败 / 随后被收回），
// 真正带着 worktree + coder 会话 + 已积累 diff 的是更早那一项。
func latestItemWithTask(ctx context.Context, store *Store, c *db.BoardCard) (*db.WorkItem, error) {
	var items []db.WorkItem
	if err := store.database.WithContext(ctx).Where("card_id = ? AND evolve_task_no <> ''", c.ID).
		Order("id DESC").Limit(1).Find(&items).Error; err != nil {
		return nil, err
	}
	if len(items) == 0 && c.WorkItemNo != "" && !IsCardRef(c.WorkItemNo) {
		if err := store.database.WithContext(ctx).Where("no = ? AND evolve_task_no <> ''", c.WorkItemNo).
			Limit(1).Find(&items).Error; err != nil {
			return nil, err
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &items[0], nil
}

// resumeCard 续接执行：把上一次（已停止但未发布的）进化单重新置 open，复用 worktree + 会话 + diff。
func resumeCard(ctx context.Context, store *Store, c *db.BoardCard, evolveDir, repoRoot string) (*RerunResult, error) {
	item, err := latestCardItem(ctx, store, c)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fmt.Errorf("该卡还没有执行记录，无法续接（请用「重新执行」）")
	}
	if item.EvolveTaskNo == "" {
		// 最新一项还没进执行链（不是调度中就是空壳）→ 只有它是"未入队"（open/triaged，不会被 dispatch 领取）时，
		// 才退一步续接更早那一轮（含 worktree/会话/diff）；否则平行再跑会重复。
		switch item.Status {
		case StatusOpen, StatusTriaged:
			if prev, perr := latestItemWithTask(ctx, store, c); perr == nil && prev != nil {
				item = prev
			}
		}
	}
	if item.EvolveTaskNo == "" {
		return nil, fmt.Errorf("最近一次尝试（%s，%s）还没进入执行链，无法续接（请用「重新执行」）", item.No, item.Status)
	}
	var task db.EvolveTask
	if err := store.database.WithContext(ctx).Where("task_no = ?", item.EvolveTaskNo).First(&task).Error; err != nil {
		return nil, fmt.Errorf("进化单 %s 不存在，无法续接（请用「重新执行」）", item.EvolveTaskNo)
	}
	switch task.Status {
	case "released":
		return nil, fmt.Errorf("该任务已审批发布（released，merge=%s），无需续接", task.MergeCommit)
	case "coding", "reviewing", "sandboxing", "testing", "approving":
		return nil, fmt.Errorf("该任务正在执行中（%s），无需续接；如需从头再来请用「重新执行」", task.Status)
	}
	// 工作目录可用性：目录在 → 直接复用；目录不在但 evolve/<task_no> 分支还在 →
	// driver 的 ensureWorktree 会从该分支重建 worktree（分支上的提交不丢），仍可续接；
	// 两者都不在 → 上一轮的代码确实没了，只能「重新执行」。
	reuse, rerr := resumeWorktreeState(evolveDir, repoRoot, task.TaskNo)
	if rerr != nil {
		return nil, rerr
	}
	// 置 open 续接（driver 下一轮拾取；worktree 复用 + session 续接 + 已积累 diff 保留）
	if task.Status != "open" {
		if err := store.database.WithContext(ctx).Model(&db.EvolveTask{}).
			Where("task_no = ?", task.TaskNo).
			Updates(map[string]any{"status": "open", "pr_error": ""}).Error; err != nil {
			return nil, err
		}
	}
	// 工作项拉到运行态 in_progress（2026-10-18 修正）：
	//   - 不能置 queued：work-dispatch 对 queued 项会**再建一个新 evolve_task**（claimAndCreateTask
	//     不检查是否已有进化单）→ 同一张卡两个任务重复跑；
	//   - 也不能停在终结态：Feedback 只扫 in_progress/in_review/approved，停 rejected 则任务跑完了
	//     工作项永远不回流（卡不回到 Done）。
	// 口径与 evolve-driver.syncWorkItemOnRun 一致（任务被判重/手动 open 时把工作项直接拉回
	// in_progress，系统驱动、不走用户态状态机）。
	if item.Status != StatusInProgress {
		if err := store.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", item.No).
			Updates(map[string]any{"status": StatusInProgress, "updated_at": time.Now()}).Error; err != nil {
			return nil, err
		}
	}
	// 卡指向的若不是被续接的这一项（最新项只是空壳）→ 把卡指回来：否则镜像轮按空壳项同步
	// （卡会显示在 Backlog 而任务实际在跑，用户看到的又是"奇怪状态"）。
	relinked := false
	if !IsCardRef(c.WorkItemNo) && c.WorkItemNo != item.No {
		if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).Where("id = ?", c.ID).
			Updates(map[string]any{"work_item_no": item.No, "updated_at": time.Now()}).Error; err != nil {
			return nil, err
		}
		relinked = true
	}
	writeRerunAudit(ctx, store, "work_item_resumed", item.No, map[string]any{
		"card_id": c.ID, "mode": RerunModeResume, "evolve_task_no": task.TaskNo,
		"prev_item_status": item.Status, "item_status": StatusInProgress,
		"prev_task_status": task.Status, "session_id": task.SessionID, "card_relinked": relinked,
		"worktree": reuse,
		"note":     "续接执行：复用 worktree + coder 会话 + 已积累 diff",
	})
	note := "已续接上一轮：复用工作目录与对话，driver 下一轮继续"
	if reuse == worktreeRebuild {
		note = fmt.Sprintf("已续接上一轮：工作目录已不存在，driver 将从分支 evolve/%s 重建后继续（对话上下文保留）", task.TaskNo)
	}
	if relinked {
		if reuse == worktreeRebuild {
			note = fmt.Sprintf("已续接上一轮（工作项 %s）：工作目录将从分支 evolve/%s 重建，并把卡重新指向它", item.No, task.TaskNo)
		} else {
			note = fmt.Sprintf("已续接上一轮（工作项 %s）：复用工作目录与对话，并把卡重新指向它", item.No)
		}
	}
	return &RerunResult{
		Mode: RerunModeResume, WorkItemNo: item.No, EvolveTaskNo: task.TaskNo, TaskStatus: "open",
		Note: note,
	}, nil
}

// 续接时工作目录的两种可用形态。
const (
	worktreeReuse   = "reuse"   // 上轮目录还在 → 直接复用
	worktreeRebuild = "rebuild" // 目录已清（driver 清理）但分支还在 → driver 重建后继续
)

// resumeWorktreeState 判断续接时工作目录能不能用（2026-10-29）：
//   - 目录在 → worktreeReuse；
//   - 目录不在但本地分支 evolve/<task_no> 还在 → worktreeRebuild
//     （driver 的 ensureWorktree 遇到目录缺失会从该分支重新 git worktree add，分支上的提交不丢）；
//   - 两者都不在 → 上一轮的代码确实没了，返回可操作的错误。
//
// 旧实现只认目录：driver 的周期清理（无未提交改动就删目录）一跑，
// 「续接执行」就永久不可用，且报错里打印的是硬编码相对路径、不是实际 stat 的路径。
func resumeWorktreeState(evolveDir, repoRoot, taskNo string) (string, error) {
	if evolveDir == "" {
		return worktreeReuse, nil
	}
	dir := filepath.Join(evolveDir, taskNo)
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return worktreeReuse, nil
	} else if err != nil && !os.IsNotExist(err) {
		// 非 ENOENT（权限/挂载异常）不能当「不存在」静默带过，否则排查不了
		return "", fmt.Errorf("检查上一轮工作目录失败（%s）：%v", dir, err)
	}
	if branchExists(repoRoot, "evolve/"+taskNo) {
		return worktreeRebuild, nil
	}
	return "", fmt.Errorf("上一轮工作目录已不存在（%s），且分支 evolve/%s 也不在了，无法续接（请用「重新执行」）", dir, taskNo)
}

// branchExists 判断仓库是否有本地分支 ref。
// 直接读 .git 下的 ref 文件，不依赖 git 二进制（API 镜像是 distroless，没有 git）。
// 先查 loose ref，再查 packed-refs（分支可能已被 gc 打包）。
func branchExists(repoRoot, branch string) bool {
	if repoRoot == "" || branch == "" {
		return false
	}
	gitDir := filepath.Join(repoRoot, ".git")
	if _, err := os.Stat(filepath.Join(gitDir, "refs", "heads", branch)); err == nil {
		return true
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		// 行格式：<sha> refs/heads/<branch>
		if f := strings.Fields(line); len(f) == 2 && f[1] == "refs/heads/"+branch {
			return true
		}
	}
	return false
}

// restartCard 重新执行：建新工作项（不继承会话）→ 从零重跑。
// 卡当前还在流水线里（queued/in_progress/in_review/approved）时拒绝，避免一张卡两个并行任务。
func restartCard(ctx context.Context, store *Store, w ScoreWeights, c *db.BoardCard) (*RerunResult, error) {
	if cur, err := latestCardItem(ctx, store, c); err == nil && cur != nil {
		switch cur.Status {
		case StatusQueued, StatusInProgress, StatusInReview, StatusApproved:
			return nil, fmt.Errorf("该卡正在流水线中（工作项 %s = %s），请等它结束或先驳回，再用「重新执行」", cur.No, cur.Status)
		}
	}
	newItem, err := createAndRelinkCard(ctx, store, w, c)
	if err != nil {
		return nil, err
	}
	if newItem == nil {
		return nil, fmt.Errorf("建卡工作项失败（看板卡 %d）", c.ID)
	}
	// 新工作项可能被入队前置校验直接判 rejected（无明确目标 / 目标不是已注册启用项目，
	// 沙箱无法执行）——此时如实告知，不要报「已入队」。
	note := "已重新入队（从零开始：新目录 + 新会话）"
	if newItem.Status != StatusQueued {
		note = fmt.Sprintf("已新建工作项 %s，但判定不可自动执行（%s，目标 %s）——卡留在 Backlog", newItem.No, newItem.Status, newItem.TargetName)
	}
	writeRerunAudit(ctx, store, "work_item_restarted", newItem.No, map[string]any{
		"card_id": c.ID, "mode": RerunModeRestart, "status": newItem.Status,
		"note": "重新执行：新工作项 + 干净 worktree + 全新会话（从零开始）",
	})
	return &RerunResult{
		Mode: RerunModeRestart, WorkItemNo: newItem.No, TaskStatus: newItem.Status,
		Note: note,
	}, nil
}

// writeRerunAudit 写重跑审计（target = 工作项号，看板执行记录可见）。
func writeRerunAudit(ctx context.Context, store *Store, action, target string, detail map[string]any) {
	b, _ := json.Marshal(detail)
	_ = store.database.WithContext(ctx).Create(&db.AuditLog{
		Actor: "board", Action: action, Target: target, Detail: string(b),
	}).Error
}
