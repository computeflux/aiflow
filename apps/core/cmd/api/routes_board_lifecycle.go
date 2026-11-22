package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workitem"
)

// registerBoardCardLifecycleRoutes 看板·卡片执行链（执行记录 / 重跑 / 手动收口 / 直接审批 /
// 跳过审核）。逻辑自 registerBoardRoutes 原样搬迁（批次 F 拆分，行为无变更）。
func registerBoardCardLifecycleRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database
	workitemStore := deps.workitemStore
	apvStore := deps.apvStore

	// S9 执行记录：卡片的所有尝试（每次 hand-off = 一个工作项 = 一轮 S1 链路）+
	// 阶段事件（audit_logs：work-dispatch 状态迁移 + evolve-driver task_status_*/coder_done/
	// reviewer_done/sandbox_done/approval_created/task_cancelled）。成功失败都可直观查看。
	r.GET("/cards/:id/executions", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var items []db.WorkItem
		if err := database.WithContext(ctx).Where("card_id = ?", id).Order("id DESC").Find(&items).Error; err != nil {
			serverError(database, "card executions: items", err)
			return
		}
		if len(items) == 0 {
			// card_id 可能为 NULL（旧数据）→ 反向查：卡的 work_item_no → 工作项
			var card db.BoardCard
			if cerr := database.WithContext(ctx).Where("id = ?", id).First(&card).Error; cerr == nil && card.WorkItemNo != "" && !strings.HasPrefix(card.WorkItemNo, "card-") {
				_ = database.WithContext(ctx).Where("no = ?", card.WorkItemNo).Order("id DESC").Find(&items).Error
			}
		}
		execs := make([]executionPayload, 0, len(items))
		for _, it := range items {
			ex := executionPayload{
				WorkItemNo: it.No, Kind: it.Kind, Status: it.Status, Title: it.Title,
				TargetName: it.TargetName,
				CreatedAt:  it.CreatedAt, PickedAt: it.PickedAt, ShippedAt: it.ShippedAt,
				EvolveTaskNo: it.EvolveTaskNo, Events: []executionEvent{},
			}
			if it.EvolveTaskNo != "" {
				var task db.EvolveTask
				if err := database.WithContext(ctx).Where("task_no = ?", it.EvolveTaskNo).First(&task).Error; err == nil {
					ex.TaskStatus = task.Status
					ex.TaskUpdatedAt = &task.UpdatedAt
					ex.TaskReview = normalizeJSONText(task.Review)
					ex.TaskDiff = task.Diff
					ex.MergeCommit = task.MergeCommit
					ex.TaskMergeState = task.MergeState
					ex.PRUrl = task.PRUrl
					ex.PRError = task.PRError
					// 人工 PR 入口：分支 evolve/<task_no>（driver 绝不 push，团队自行推分支/建 PR）
					ex.Branch = "evolve/" + task.TaskNo
					ex.PushCmd = "git push origin " + ex.Branch
					ex.CompareURL = gitRemoteURL() + "/compare/" + gitBaseBranch() + "..." + ex.Branch + "?expand=1"
				}
			}
			// 事件：工作项迁移（target=wi-...）+ 进化链阶段（target=WI-...）合并按时间序
			targets := []string{it.No}
			if it.EvolveTaskNo != "" {
				targets = append(targets, it.EvolveTaskNo)
			}
			var logs []db.AuditLog
			if err := database.WithContext(ctx).Where("target IN ?", targets).Order("id ASC").Find(&logs).Error; err == nil {
				for _, l := range logs {
					ex.Events = append(ex.Events, executionEvent{Action: l.Action, Actor: l.Actor, At: l.CreatedAt, Detail: l.Detail, Note: l.Note})
					// 提取 coder 会话 ID（coder_start / coder_done 事件 detail 含 session_id）
					// coder_start 在 coder 启动前记录 → "进行中"时即可在 pi-web 实时查看
					if (l.Action == "coder_start" || l.Action == "coder_done") && ex.CoderSession == "" {
						var det map[string]any
						if json.Unmarshal([]byte(l.Detail), &det) == nil {
							if sid, ok := det["session_id"].(string); ok {
								ex.CoderSession = sid
							}
						}
					}
				}
			}
			execs = append(execs, ex)
		}
		ctx.JSON(http.StatusOK, gin.H{"card_id": id, "executions": execs})
	})

	// S9 失败重跑（看板按钮，2026-10-18）：两种模式
	//   mode=resume  续接执行：复用上一轮进化单（同 worktree + 同 coder 会话 + 已积累 diff）→ 置 open 让 driver 继续；
	//   mode=restart 重新执行：建新工作项 + 新进化单（干净 worktree + 全新会话，从零开始）→ 立即领取入队。
	// 背景：失败后原先只能从零重跑（新工作项新目录新会话），环境性失败（LLM 抖动/超时/driver 重启/审核打回）
	// 想把上一轮的代码与对话续上时无路可走。
	r.POST("/cards/:id/rerun", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var card db.BoardCard
		if err := database.WithContext(ctx).First(&card, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
			return
		}
		var body struct {
			Mode string `json:"mode"`
		}
		_ = ctx.ShouldBindJSON(&body)
		repoRoot := apiEnvOr("REPO_ROOT", "/repo")
		evolveDir := filepath.Join(repoRoot, apiEnvOr("EVOLVE_CACHE_DIR", "cache/evolve"))
		res, rerr := workitem.RerunCard(ctx, workitemStore, workitem.DefaultWeights(), &card, body.Mode, evolveDir, repoRoot)
		if rerr != nil {
			ctx.JSON(http.StatusConflict, gin.H{"error": rerr.Error()})
			return
		}
		if body.Mode == workitem.RerunModeRestart && res.TaskStatus == "queued" {
			// 立即领取（并发满时会失败 —— 不要紧，dispatch 轮会捡起）
			if _, perr := workitem.PickItem(ctx, workitemStore, res.WorkItemNo); perr != nil {
				log.Printf("card %d rerun(restart): 即时领取未成功（等 dispatch 轮）：%v", id, perr)
			}
		}
		// 即时反馈：把卡状态/列对齐到工作项（不等 5min 镜像轮）
		if serr := workitem.SyncOneCardStatus(ctx, workitemStore, card.ID, res.WorkItemNo); serr != nil {
			log.Printf("WARN: card %d rerun status sync: %v", id, serr)
		}
		log.Printf("card %d rerun mode=%s → work item %s (task=%s)", id, res.Mode, res.WorkItemNo, res.EvolveTaskNo)
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "result": res})
	})

	// S9 看板：人工结束在跑任务（2026-10-28）。
	// 背景：in_progress 卡直接拖 Done 会翻译成 in_progress → shipped，工作项状态机禁止跳级 →
	// 被拒后 5min 镜像又把卡片拉回去，用户无法手动结束在跑任务。本接口按合法阶梯收口：
	//   outcome=done   → 停任务 + in_progress → in_review → approved → shipped（卡 → Done）
	//   outcome=reject → 停任务 + in_progress → rejected（卡 → Backlog，可重新 hand-off 重跑）
	// 停任务：待审批单先 reject（复用 approval.Store.Decide 的联动取消）；其余运行态直接置 cancelled。
	// 驱动器在轮次边界检查 cancelled 后退出（LLM 调用中途无法安全打断）。
	r.POST("/cards/:id/finish", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var card db.BoardCard
		if err := database.WithContext(ctx).First(&card, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
			return
		}
		var body struct {
			Outcome string `json:"outcome"` // done | reject | review
			Reason  string `json:"reason"`
		}
		_ = ctx.ShouldBindJSON(&body)
		outcome := strings.TrimSpace(body.Outcome)
		var target, cardStatus, targetList string
		switch outcome {
		case "done":
			target, cardStatus, targetList = workitem.StatusShipped, "done", "Done"
		case "reject":
			target, cardStatus, targetList = workitem.StatusRejected, "todo", "Backlog"
		case "review":
			// 「完成执行」（2026-11-03）：停掉当前任务，直接把工作项推成 in_review → 卡进 Review 列，
			// 由人工看 diff。用于失败/卡住的任务：不必等流水线自己跑到 approving。
			target, cardStatus, targetList = workitem.StatusInReview, "in_review", "Review"
		default:
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "outcome must be done|reject|review"})
			return
		}

		// 关联工作项：优先 card.work_item_no（真实工作项号），否则按 card_id 反查最新
		q := database.WithContext(ctx).Where("card_id = ?", card.ID)
		if card.WorkItemNo != "" && !strings.HasPrefix(card.WorkItemNo, "card-") {
			q = database.WithContext(ctx).Where("no = ?", card.WorkItemNo)
		}
		var item db.WorkItem
		if err := q.Order("id DESC").First(&item).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "该卡没有关联工作项（尚未交给工作流）"})
			return
		}
		if workitem.IsTerminal(item.Status) {
			// 例外：「完成执行」允许把已驳回的任务拉回 Review —— 只要它执行过（有 diff/审查记录），
			// 人工就有理由直接看改动，而不是被迫重跑一遍。
			if !(outcome == "review" && item.Status == workitem.StatusRejected) {
				ctx.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("工作项已终结（%s），无法继续", item.Status)})
				return
			}
		}

		actor := authn.UserID(ctx)
		if strings.TrimSpace(actor) == "" {
			actor = "board"
		}
		reason := strings.TrimSpace(body.Reason)
		if reason == "" {
			reason = "看板手动结束"
		}

		// 已合入的改动不能“终止”，只能验收完成
		var task db.EvolveTask
		hasTask := false
		if item.EvolveTaskNo != "" {
			if err := database.WithContext(ctx).Where("task_no = ?", item.EvolveTaskNo).First(&task).Error; err == nil {
				hasTask = true
			}
		}
		if outcome == "reject" && hasTask && task.Status == "released" {
			ctx.JSON(http.StatusConflict, gin.H{"error": "任务已合入 main，无法终止；如需收口请选择「验收完成」"})
			return
		}
		// 红线（2026-11-03）：**代码必须合并到 main 才能进 Done**。
		// 没有进化单（无法证明合并）或 merge_commit 为空 → 拒绝验收，卡继续停在 Review。
		// 典型场景：人工「完成执行」停掉了任务，但代码根本没走合并。
		if outcome == "done" {
			if !hasTask || strings.TrimSpace(task.MergeCommit) == "" {
				ctx.JSON(http.StatusConflict, gin.H{"error": "代码还没合并到 main，不能进 Done。请先把改动合入主分支（等 driver 合入，或人工合并后重新执行），卡会继续停在 Review"})
				return
			}
		}

		// 1. 停掉在跑任务：待审批单先 reject（Decide 会联动把 approving 任务置 cancelled）
		if hasTask && task.ApprovalID != nil {
			var apr db.Approval
			if err := database.WithContext(ctx).First(&apr, *task.ApprovalID).Error; err == nil && apr.Status == "pending" {
				if _, derr := apvStore.Decide(ctx, int(apr.ID), "reject", actor, reason); derr != nil {
					log.Printf("WARN: finish card %d: reject approval %d: %v", card.ID, apr.ID, derr)
				}
			}
		}
		if hasTask {
			// 停掉在跑任务。
			// outcome=review 用 "stopped" 而不是 "cancelled"：cancelled 会被 Feedback/看板投影
			// 当成 rejected 拉回 Backlog，而我们要让卡留在 Review 等人工看 diff。
			stopStatus := "cancelled"
			if outcome == "review" {
				stopStatus = "stopped"
			}
			if res := database.WithContext(ctx).Model(&db.EvolveTask{}).
				Where("task_no = ? AND status NOT IN ?", item.EvolveTaskNo,
					[]string{"released", "cancelled", "rolled_back", "expired"}).
				Update("status", stopStatus); res.Error != nil {
				log.Printf("WARN: finish card %d: stop task %s: %v", card.ID, item.EvolveTaskNo, res.Error)
			}
		}

		// 2. 工作项按合法阶梯收口
		path, ferr := workitem.FinishTo(ctx, workitemStore, item.No, target)
		if ferr != nil {
			ctx.JSON(http.StatusConflict, gin.H{"error": ferr.Error()})
			return
		}

		// 3. 卡片回流到同看板的目标列（列不存在时只改状态）
		updates := map[string]any{"status": cardStatus, "updated_at": time.Now()}
		var curList db.BoardList
		if err := database.WithContext(ctx).First(&curList, card.ListID).Error; err == nil {
			names := []string{targetList}
			if targetList == "Backlog" {
				names = append(names, "Todo") // 没有 Backlog 列时退到 Todo
			}
			for _, n := range names {
				var dest db.BoardList
				if err := database.WithContext(ctx).Where("board_id = ? AND name = ?", curList.BoardID, n).First(&dest).Error; err == nil {
					updates["list_id"] = dest.ID
					break
				}
			}
		}
		if uerr := database.WithContext(ctx).Model(&db.BoardCard{}).Where("id = ?", card.ID).Updates(updates).Error; uerr != nil {
			serverError(database, "finish card", uerr)
			return
		}

		apvStore.Log(ctx, actor, "card_finished_manually", fmt.Sprintf("card-%d", card.ID), map[string]any{
			"outcome": outcome, "work_item": item.No, "status_path": path,
			"evolve_task": item.EvolveTaskNo, "reason": reason,
		})
		log.Printf("card %d manually finished: outcome=%s work_item=%s path=%v", card.ID, outcome, item.No, path)
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "work_item": item.No, "status": target, "path": path})
	})

	cardDecisionHandler := func(decision string) gin.HandlerFunc {
		return func(ctx *gin.Context) {
			id, _ := strconv.Atoi(ctx.Param("id"))
			ap, err := findCardPendingApproval(ctx, database, id)
			if err != nil {
				serverError(database, "card approval lookup", err)
				return
			}
			if ap == nil {
				id, _ := strconv.Atoi(ctx.Param("id"))
				if decision != "approve" {
					ctx.JSON(http.StatusNotFound, gin.H{"error": "该卡当前无待审批单（工作项未进入审批 / 已决策 / 非自动审批目标）"})
					return
				}
				// 无审批单的放行（2026-11-03）：「完成执行」把卡推到 Review 后，没有走审批链。
				// 红线是「代码必须合并才能 Done」→ 这里不直接进 Done，而是把进化单置 released，
				// 让 evolve-driver 的 mergeToMain 把分支合入 main；合入成功（merge_commit 非空）后
				// 回流轮才会把卡推到 Done。合入失败则一直停在 Review。
				var card db.BoardCard
				if err := database.WithContext(ctx).First(&card, id).Error; err != nil {
					ctx.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
					return
				}
				q := database.WithContext(ctx).Where("card_id = ?", card.ID)
				if card.WorkItemNo != "" && !strings.HasPrefix(card.WorkItemNo, "card-") {
					q = database.WithContext(ctx).Where("no = ?", card.WorkItemNo)
				}
				var item db.WorkItem
				if err := q.Order("id DESC").First(&item).Error; err != nil || strings.TrimSpace(item.EvolveTaskNo) == "" {
					ctx.JSON(http.StatusConflict, gin.H{"error": "该卡没有关联进化单，无法合并代码；如需结案请手工处理"})
					return
				}
				var task db.EvolveTask
				if err := database.WithContext(ctx).Where("task_no = ?", item.EvolveTaskNo).First(&task).Error; err != nil {
					ctx.JSON(http.StatusConflict, gin.H{"error": "进化单不存在，无法合并代码"})
					return
				}
				if strings.TrimSpace(task.MergeCommit) != "" {
					ctx.JSON(http.StatusOK, gin.H{"ok": true, "note": "代码已合并（" + task.MergeCommit[:min(8, len(task.MergeCommit))] + "），等回流轮推到 Done"})
					return
				}
				if task.Status != "released" {
					if err := database.WithContext(ctx).Model(&db.EvolveTask{}).
						Where("task_no = ?", task.TaskNo).
						Updates(map[string]any{"status": "released", "fail_reason": ""}).Error; err != nil {
						serverError(database, "release task for merge", err)
						return
					}
				}
				actor := authn.UserID(ctx)
				if strings.TrimSpace(actor) == "" {
					actor = "board"
				}
				apvStore.Log(ctx, actor, "card_approved_for_merge", fmt.Sprintf("card-%d", card.ID), map[string]any{
					"work_item": item.No, "evolve_task": task.TaskNo, "prev_task_status": task.Status,
				})
				ctx.JSON(http.StatusOK, gin.H{"ok": true, "note": "已放行：驱动会把它合入 main（约 5 分钟一次），合入成功后才进 Done"})
				return
			}
			approver := authn.UserID(ctx)
			if strings.TrimSpace(approver) == "" {
				approver = "board"
			}
			var body struct {
				Reason string `json:"reason"`
			}
			_ = ctx.ShouldBindJSON(&body)
			if _, derr := apvStore.Decide(ctx, int(ap.ID), decision, approver, body.Reason); derr != nil {
				switch {
				case errors.Is(derr, approval.ErrApprovalDecided):
					ctx.JSON(http.StatusConflict, gin.H{"error": derr.Error()})
				default:
					ctx.JSON(http.StatusBadGateway, gin.H{"error": derr.Error()})
				}
				return
			}
			ctx.JSON(http.StatusOK, gin.H{"ok": true})
		}
	}

	// 看板「跳过审核」（2026-11-03）：给进化单打 skip_review 标记，evolve-driver 在 reviewer 阶段
	// 直接越过 ocr，继续沙箱/CI → 审批 → 卡进 Review 列由人工看 diff。
	//
	// 适用：diff 过大（一次 vendor 上万行）或 reviewer 反复超时的任务 ——
	// 逐行自动审这类改动只会耗满 EVOLVE_MAX_ROUNDS（默认 10 轮 × 1h）再 cancel，不如直接交人。
	//
	// 注意：标记是「下一轮生效」。如果 reviewer 正在跑，要等本轮结束（中途无法安全打断 LLM 调用）。
	r.POST("/cards/:id/skip-review", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var card db.BoardCard
		if err := database.WithContext(ctx).First(&card, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
			return
		}
		// 关联工作项：优先 card.work_item_no（真实工作项号），否则按 card_id 反查最新
		q := database.WithContext(ctx).Where("card_id = ?", card.ID)
		if card.WorkItemNo != "" && !strings.HasPrefix(card.WorkItemNo, "card-") {
			q = database.WithContext(ctx).Where("no = ?", card.WorkItemNo)
		}
		var item db.WorkItem
		if err := q.Order("id DESC").First(&item).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "该卡没有关联工作项（尚未交给工作流）"})
			return
		}
		if strings.TrimSpace(item.EvolveTaskNo) == "" {
			ctx.JSON(http.StatusConflict, gin.H{"error": "该卡还没有进化单（尚未开始执行）；交给工作流后再点「跳过审核」"})
			return
		}
		var task db.EvolveTask
		if err := database.WithContext(ctx).Where("task_no = ?", item.EvolveTaskNo).First(&task).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "进化单不存在"})
			return
		}
		updates := map[string]any{"skip_review": true}
		// 失败/取消的任务：打标记的同时置回 open，让 driver 下一轮接着跑（一键「忽略审核并续跑」）。
		// 运行中的任务不改状态：本轮 reviewer 结束后下一轮自动生效（LLM 调用中无法安全打断）。
		resumed := false
		switch task.Status {
		case "failed", "cancelled":
			updates["status"] = "open"
			updates["fail_reason"] = ""
			resumed = true
		}
		if err := database.WithContext(ctx).Model(&db.EvolveTask{}).
			Where("task_no = ?", task.TaskNo).Updates(updates).Error; err != nil {
			serverError(database, "skip review", err)
			return
		}
		actor := authn.UserID(ctx)
		if strings.TrimSpace(actor) == "" {
			actor = "board"
		}
		apvStore.Log(ctx, actor, "review_skip_requested", task.TaskNo, map[string]any{
			"card_id": card.ID, "work_item_no": item.No, "task_status": task.Status, "resumed": resumed,
		})
		note := "已标记跳过 AI 审核：下一轮 reviewer 阶段直接越过，继续沙箱/CI 后进 Review 列；若当前正在 reviewer 中，需等本轮结束"
		if resumed {
			note = "已标记跳过 AI 审核并重新入队，driver 会接着跑（沙箱/CI）后进 Review 列"
		}
		ctx.JSON(http.StatusOK, gin.H{
			"ok":          true,
			"task_no":     task.TaskNo,
			"task_status": task.Status,
			"resumed":     resumed,
			"note":        note,
		})
	})

	r.POST("/cards/:id/approve", requireRole("admin", "board_approver"), cardDecisionHandler("approve"))

	r.POST("/cards/:id/reject", requireRole("admin", "board_approver"), cardDecisionHandler("reject"))
}
