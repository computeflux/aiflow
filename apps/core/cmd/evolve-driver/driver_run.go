package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
)

func (d *driver) runTask(task *db.EvolveTask) {
	d.setRunning(task.TaskNo)
	defer d.setRunning("")
	// 看板「结束任务」可能已经把它置 cancelled → 直接退出（不建 worktree、不覆盖状态）
	if d.taskCancelledExternally(task.TaskNo) {
		log.Printf("evolve %s: 已被看板手动结束（cancelled）→ 跳过执行", task.TaskNo)
		return
	}
	// 目标以卡上当前选择为准（工作项/进化单只是建单时快照）：开跑即同步，
	// 避免后续日志/沙箱/审批继续用旧目标；每轮范围校验前还会再同步（用户可能运行中补勾目标）。
	d.syncTaskTargetFromCard(task)
	// 1. worktree（绝不 push/merge）
	workdir, err := d.ensureWorktree(task)
	if err != nil {
		d.failTask(task, "worktree", err.Error())
		return
	}
	task.Workdir = workdir
	// 任务重启（无 session = 全新开始）→ 重置 worktree 到干净状态：
	// 上次运行残留的未提交代码会让 agent 误判"已完成"→ empty diff。
	// 轮次重试（round 2+）不走这里（runTask 只调一次）。
	if task.SessionID == "" {
		d.resetWorktree(workdir)
	}
	// 红线：只有**成功合并**（merge-success）才清理 worktree；失败/终止态目录一律保留
	// （「续接执行」依赖目录 + 会话）。若上次成功收尾在 merge-success 之前崩溃留下孤儿目录，
	// 重跑前先清掉——否则 reset 到已合并的分支 HEAD = 上一轮代码复活，coder 会误判已完成 → empty diff。
	if d.cleanupMergedWorktree(task) {
		workdir, err = d.ensureWorktree(task)
		if err != nil {
			d.failTask(task, "worktree", err.Error())
			return
		}
		task.Workdir = workdir
	}
	// worktree 健康自检（2026-11-10 WI-1794313637-b6264a94 教训：目录被外部重建后
	// .git 指针失效，变更检测永远为空 → 连烧 20 轮 empty-diff）：git 不可用 → 删掉重建一次；
	// 重建后仍不可用 → fail-fast（明确报 worktree 故障，不再空烧轮次）。
	// 子仓库嵌套 worktree 不健康（如目录被手工替换成普通拷贝）→ 单独拆掉重接，不伤主目录。
	if bad := d.unhealthySubWorktrees(workdir); len(bad) > 0 {
		log.Printf("evolve %s: 子仓库 worktree 异常（%v）→ 拆除重建", task.TaskNo, bad)
		d.rebuildSubWorktrees(task, workdir, bad)
		if bad2 := d.unhealthySubWorktrees(workdir); len(bad2) > 0 {
			d.failTask(task, "worktree", fmt.Sprintf("子仓库 worktree 重建后仍不可用：%v", bad2))
			return
		}
	}
	if !d.worktreeHealthy(workdir) {
		d.cleanupWorktree(task.TaskNo, "unhealthy-rebuild")
		workdir, err = d.ensureWorktree(task)
		if err != nil {
			d.failTask(task, "worktree", err.Error())
			return
		}
		task.Workdir = workdir
		if !d.worktreeHealthy(workdir) {
			d.failTask(task, "worktree", fmt.Sprintf("worktree 重建后 git 仍不可用：%s", workdir))
			return
		}
	}
	d.setStatus(task, "coding")
	// 看板侧对齐（幂等）：任务可能被手动/脚本重新 open（打回重跑），而关联工作项已终结（rejected）
	// → 看板看不到在跑的任务，且此时用户再 hand-off 会重复建任务。
	d.syncWorkItemOnRun(task)

	// 2–6. coder → 范围校验 → reviewer → 沙箱 → CI（最多 MaxRounds 轮：
	// empty diff / reviewer error / 沙箱失败 / CI 未全绿 均触发下一轮，把失败信息反馈给 coder 重改；
	// 全绿才 break 进入构建/审批。达上限仍未全绿 → 打回人工。
	var coderOut *coderOut
	var reviewJSON string
	var ciOut map[string]any
	lastFeedback := "" // 上一轮失败原因（反馈给 coder 重改）
	allGreen := false
	// 任务重新执行时复用上一轮的 pi-web 会话（task.SessionID 持久化）：
	// 对话上下文 + worktree 代码都保留，coder 从上次中断处继续，不重复发送任务描述。
	// 若 session 在 pi-web 已不存在，adapter 自动回退新建（FULL_PROMPT）。
	sessionID := task.SessionID
	for attempt := 1; attempt <= d.cfg.MaxRounds; attempt++ {
		// 每轮开始先看是否已被看板「结束任务」取消（长阶段只能在轮次边界响应）
		if d.taskCancelledExternally(task.TaskNo) {
			log.Printf("evolve %s: 已被看板手动结束（cancelled）→ 停止执行（round %d）", task.TaskNo, attempt)
			d.store.Log(d.ctx(), "evolve-driver", "task_cancel_observed", task.TaskNo, map[string]any{"round": attempt})
			return
		}
		feedback := ""
		if attempt > 1 {
			feedback = "\n\n" + lastFeedback
		}
		// coder 同样要能在「任务被外部完成」时被杀（pi 单次超时 2 小时，不杀会堵死串行队列）
		coderCtx, cancelCoder := d.watchTaskFinished(task.TaskNo)
		d.store.Log(d.ctx(), "evolve-driver", "coder_start", task.TaskNo, map[string]any{
			"session_id": sessionID, "round": attempt,
		})
		out, err := d.runCoderCtx(coderCtx, task, feedback, sessionID)
		cancelCoder()
		if err != nil {
			if errors.Is(err, errTaskFinishedExternally) {
				log.Printf("evolve %s: 任务已被外部完成/停止 → 放弃本轮 coder，释放队列", task.TaskNo)
				d.store.Log(d.ctx(), "evolve-driver", "task_finished_during_coder", task.TaskNo, map[string]any{"round": attempt})
				return
			}
			d.cancel(task, "coder: "+err.Error())
			return
		}
		coderOut = out
		task.Diff = out.Diff
		// 记录 pi-web 真实 session ID：本轮后续轮次续接 + 持久化供任务重新执行时复用
		if out.SessionID != "" {
			sessionID = out.SessionID
			task.SessionID = out.SessionID
		}
		d.save(task) // 立即落库：后续阶段失败也不丢 diff / session
		// 空 diff：未达上限给下一轮机会（带反馈重试），达上限才 fail。
		// 诊断留痕：把会话文件里 edit/write 工具调用的目标路径记进 audit——子仓库（apps/<name>）
		// 改动在 worktree 重建后会丢，没有这份清单就无法事后定位「coder 说改了、diff 却是空」。
		if strings.TrimSpace(out.Diff) == "" && len(out.FilesChanged) == 0 {
			d.store.Log(d.ctx(), "evolve-driver", "coder_empty_diff", task.TaskNo, map[string]any{
				"round": attempt, "session_id": sessionID, "workdir": workdir,
				"tool_writes": d.sessionWriteTargets(sessionID),
			})
			// 交叉校验：会话里 edit/write 的路径落在**仓库外**（如 pi-web 把 cwd 解析成容器路径
			// /repo/...、而宿主上不存在）→ agent 在写一个不存在的工作区，重试必然空转
			// → 立即失败并给出定位（2026-11-10 WI-1794313637：连烧 20 轮才到上限）。
			if outside := d.writesOutsideWorkdir(task, workdir, sessionID); len(outside) > 0 {
				d.failTask(task, "empty-diff", fmt.Sprintf(
					"coder 的 edit/write 落在 worktree（%s）之外：%s（会话 cwd 与 driver worktree 不一致，重试无效，检查 pi-web cwd 解析/挂载）",
					workdir, strings.Join(outside, ", ")))
				return
			}
			if attempt < d.cfg.MaxRounds {
				lastFeedback = "上一轮你没有修改任何文件（empty diff）。请仔细分析任务要求，定位问题代码，实际修改文件。不要只输出分析，必须用编辑工具修改代码。"
				log.Printf("evolve %s: empty diff in round %d, retrying with feedback (round %d/%d)", task.TaskNo, attempt, attempt+1, d.cfg.MaxRounds)
				continue
			}
			d.failTask(task, "empty-diff", fmt.Sprintf("coder 未产生任何变更（%d 轮后仍无变更）", d.cfg.MaxRounds))
			return
		}

		// 3. 写范围校验（小范围越界放行：≤N 文件且 >0 且 ≤M 行 → 放行留痕；超阈值打回人工 PR）
		// 先用卡上最新目标刷新任务：工作项/进化单的目标只是建单时快照，用户后补勾选的目标
		//（如 core）必须在本轮生效，否则合法 diff 会被按旧目标判越界。
		d.syncTaskTargetFromCard(task)
		sc := d.checkScope(task, out)
		if sc.Violation != "" {
			_ = d.store.CreateIntervention(d.ctx(), task.TargetName, "manual",
				fmt.Sprintf("进化单 %s 越界打回：%s", task.TaskNo, sc.Violation))
			d.notify.SendAlert(d.ctx(), notify.Alert{
				Title: fmt.Sprintf("进化单越界 %s", task.TaskNo), Body: sc.Violation, Level: "critical"})
			d.cancel(task, "scope violation: "+sc.Violation)
			return
		}
		if len(sc.Allowed) > 0 {
			// 小范围越界放行：审计留痕（reviewer + 人工审批仍会看到 diff）
			d.store.Log(d.ctx(), "evolve-driver", "scope_allow", task.TaskNo, map[string]any{
				"files": sc.Allowed, "lines": sc.AllowedLines,
				"limit_files": d.cfg.ScopeAllowFiles, "limit_lines": d.cfg.ScopeAllowLines,
			})
			log.Printf("evolve %s: 小范围越界放行（%d 文件/%d 行 ≤ 阈值 %d/%d）：%s",
				task.TaskNo, len(sc.Allowed), sc.AllowedLines, d.cfg.ScopeAllowFiles, d.cfg.ScopeAllowLines,
				strings.Join(sc.Allowed, ", "))
		}

		// 4. reviewer（执行失败按退避重试；审核意见走下面的 hasErr 分支反馈 coder 重改）
		// （coder 期间可能已被手动结束 → 调 reviewer 前再确认一次）
		if d.taskCancelledExternally(task.TaskNo) {
			log.Printf("evolve %s: 已被看板手动结束（cancelled）→ 跳过 reviewer（round %d）", task.TaskNo, attempt)
			d.store.Log(d.ctx(), "evolve-driver", "task_cancel_observed", task.TaskNo, map[string]any{"round": attempt})
			return
		}
		revOut, revRaw, err := d.runReviewerWithRetry(task, out)
		if err != nil {
			// 任务已被外部完成（审批合入 / 人工结束）：不当作失败，直接退出释放队列
			if errors.Is(err, errTaskFinishedExternally) {
				log.Printf("evolve %s: 任务已被外部完成/停止 → 放弃本轮 reviewer，释放队列", task.TaskNo)
				d.store.Log(d.ctx(), "evolve-driver", "task_finished_during_review", task.TaskNo, map[string]any{"round": attempt})
				return
			}
			// 重试耗尽仍不可用：打回人工（intervention + 告警），不当作「代码被拒」
			d.rejectToHuman(task, "reviewer", err.Error())
			return
		}
		// 生成产物兜底过滤：适配器已按排除模式过滤；这里防其它 reviewer adapter 未过滤，
		// 避免生成文件（lock/dist/ABI 绑定/.env）的评论把任务卡在重修循环。
		patterns := d.reviewExcludePatterns()
		if kept, ignored := filterGeneratedComments(revOut.Comments, patterns); ignored > 0 {
			revOut.Comments = kept
			if b, mErr := json.Marshal(map[string]any{"ok": true, "comments": kept}); mErr == nil {
				revRaw = string(b)
			}
			log.Printf("evolve %s: reviewer %d 条评论落在生成文件上 → 已忽略（不参与打回）", task.TaskNo, ignored)
			d.store.Log(d.ctx(), "evolve-driver", "review_ignored_generated", task.TaskNo, map[string]any{
				"ignored": ignored, "patterns": patterns,
			})
		}
		reviewJSON = revRaw
		task.Review = revRaw
		d.save(task)
		// 只把 **error 级**评论反馈给 coder：warning 不要求修复，混进反馈会让 coder
		// 反复去改生成产物/无关文件（2026-10-28 实测：39 条评论里仅 2 条 error，任务却在重修轮里打转）。
		errComments := make([]reviewComment, 0, 4)
		for _, c := range revOut.Comments {
			if c.Level == "error" {
				errComments = append(errComments, c)
			}
		}
		hasErr := len(errComments) > 0
		errReviewJSON := reviewJSON
		if b, mErr := json.Marshal(map[string]any{"ok": true, "comments": errComments}); mErr == nil {
			errReviewJSON = string(b)
		}
		if hasErr {
			if attempt == d.cfg.MaxRounds {
				_ = d.store.CreateIntervention(d.ctx(), task.TargetName, "manual",
					fmt.Sprintf("进化单 %s 审核打回（%d 轮后转人工）：\n%s", task.TaskNo, d.cfg.MaxRounds, errReviewJSON))
				d.notify.SendAlert(d.ctx(), notify.Alert{
					Title: fmt.Sprintf("进化单审核打回 %s", task.TaskNo), Body: fmt.Sprintf("reviewer error 级评论，%d 轮自动修复后仍未通过，转人工处理。", d.cfg.MaxRounds), Level: "critical"})
				d.failTask(task, "review", fmt.Sprintf("审核打回（%d 轮后仍未通过）：%s", d.cfg.MaxRounds, errReviewJSON))
				return
			}
			lastFeedback = "上一轮 reviewer 的 error 级评论（必须逐条修复）：\n" + errReviewJSON
			log.Printf("evolve %s: reviewer found %d error-level comments, auto-retry (round %d/%d)", task.TaskNo, len(errComments), attempt+1, d.cfg.MaxRounds)
			continue
		}

		// reviewer 可能跑几十分钟，期间可能被看板「结束任务」→ 进沙箱前再确认
		if d.taskCancelledExternally(task.TaskNo) {
			log.Printf("evolve %s: 已被看板手动结束（cancelled）→ 跳过沙箱/CI（round %d）", task.TaskNo, attempt)
			d.store.Log(d.ctx(), "evolve-driver", "task_cancel_observed", task.TaskNo, map[string]any{"round": attempt, "stage": "pre-sandbox"})
			return
		}

		// 5. 沙箱：目标项目单测（--network none、/work 只读）
		// 目标项目未注册 / 无测试命令 / 无镜像 → 跳过（留痕），继续 CI 全量把关。
		sandboxResp, sandboxSkipped, err := d.runSandbox(task)
		if err != nil {
			// runner 通信/运行错误（非代码问题）→ 打回人工
			d.rejectToHuman(task, "sandbox", err.Error())
			return
		}
		sandboxFail := ""
		if sandboxSkipped {
			// 留痕（audit）：目标项目未注册/无测试配置 → 沙箱跳过，CI 全量兜底
			d.store.Log(d.ctx(), "evolve-driver", "sandbox_skipped", task.TaskNo, map[string]any{
				"target": task.TargetName, "reason": "project not registered or no sandbox config",
			})
			log.Printf("evolve %s: 沙箱跳过（目标项目未注册/无测试配置）→ 留痕不拦，继续 CI", task.TaskNo)
		} else if sandboxResp.ExitCode != 0 {
			if isPytestNoTests(sandboxResp.cmd, sandboxResp.ExitCode) {
				// 与 ci-runner.sh 同口径：pytest exit 5 = 未收集到用例 → 留痕不拦
				d.store.Log(d.ctx(), "evolve-driver", "sandbox_no_tests", task.TaskNo, map[string]any{
					"exit_code": 5, "cmd": strings.Join(sandboxResp.cmd, " "), "stdout_tail": tail(sandboxResp.StdoutTail, 300),
				})
				log.Printf("evolve %s: 沙箱 pytest 未收集到用例（exit 5）→ 留痕不拦（同 ci-runner 口径）", task.TaskNo)
			} else {
				sandboxFail = fmt.Sprintf("上一轮沙箱单测失败（exit_code=%d），请修复代码使测试通过：\n%s",
					sandboxResp.ExitCode, tail(sandboxResp.StderrTail, 1500))
			}
		}

		// 6. CI 全量；未全绿 → 反馈 coder 重改
		ciOut2, ciErr := d.runCI(task)
		ciFail := ""
		if ciErr != nil {
			ciFail = "上一轮 CI 未全绿（构建/测试失败），请修复代码使 CI 通过：\n" + tail(ciErr.Error(), 1500)
		}

		if sandboxFail != "" || ciFail != "" {
			if attempt == d.cfg.MaxRounds {
				reason := strings.TrimSpace(sandboxFail + "\n" + ciFail)
				_ = d.store.CreateIntervention(d.ctx(), task.TargetName, "manual",
					fmt.Sprintf("进化单 %s 测试打回（%d 轮后转人工）：\n%s", task.TaskNo, d.cfg.MaxRounds, reason))
				d.notify.SendAlert(d.ctx(), notify.Alert{
					Title: fmt.Sprintf("进化单测试打回 %s", task.TaskNo),
					Body:  fmt.Sprintf("沙箱/CI 未全绿，%d 轮自动修复后仍未通过，转人工处理。", d.cfg.MaxRounds),
					Level: "critical"})
				d.rejectToHuman(task, "ci", tail(reason, 500))
				return
			}
			lastFeedback = strings.TrimSpace(sandboxFail + "\n" + ciFail)
			log.Printf("evolve %s: sandbox/CI not green, auto-retry (round %d/%d)", task.TaskNo, attempt+1, d.cfg.MaxRounds)
			continue
		}

		// 全绿 → 进入构建/审批
		ciOut = ciOut2
		allGreen = true
		break
	}
	if !allGreen {
		// 兜底（循环内达上限已 return，正常不会到这里）
		d.rejectToHuman(task, "ci", "max rounds reached without green CI")
		return
	}

	// 审核报告 JSON 同目录（§3.6：~/.cache/ci/<task_no>.review.json）
	_ = os.WriteFile(d.cfg.CacheAbs()+"/"+task.TaskNo+".review.json", []byte(task.Review), 0o644)
	testReport := map[string]any{
		"junit":    ciOut["junit"],
		"passed":   ciOut["passed"],
		"failed":   ciOut["failed"],
		"contract": ciOut["contract"],
	}
	task.TestReport = mustJSON(testReport)
	d.save(task)

	// 6.5 构建目标镜像（灰度要跑真新代码）：从 worktree 构建 <app>:<version>
	//（无 Dockerfile / 目标未注册 → 跳过留痕，灰度回退继承 live 镜像）
	image, err := d.buildTargetImage(task)
	if err != nil {
		d.rejectToHuman(task, "build", err.Error())
		return
	}

	// 7. 审批单（kind=skill_release，payload = diff + review + test_report + image）
	if err := d.createReleaseApproval(task, coderOut, testReport, image); err != nil {
		d.rejectToHuman(task, "approval", err.Error())
		return
	}
	log.Printf("evolve %s: all green → approval created, awaiting human", task.TaskNo)
}
