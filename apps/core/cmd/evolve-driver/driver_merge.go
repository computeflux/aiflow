package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// maybePublish 发布跟进（EVOLVE_MERGE_MODE）：
//   - local（默认）：已上线（released）→ 本地快进 main（无需 token）；
//   - pr：审批通过/已上线 → 推分支 + 建 GitHub PR；
//   - both：两者都做；off：不做（人工）。
func (d *driver) maybePublish() {
	switch d.cfg.MergeMode {
	case "off":
		return
	case "pr":
		d.createPRs()
	case "both":
		d.createPRs()
		d.mergeToMain()
	default: // local
		d.mergeToMain()
	}
}

// mergePaused 判断「base 工作区不干净」的暂缓态是否仍在生效（untilUnixNano = 0 表示未暂停）。
func mergePaused(untilUnixNano int64, now time.Time) bool {
	return untilUnixNano > 0 && now.UnixNano() < untilUnixNano
}

// mergeToMain 把已上线（released）的 evolve 分支合并到本地 main（EVOLVE_MERGE_MODE=local）：
// 快进则直接 CAS update-ref（只动 ref，不碰工作区）；非快进（main 已被前序任务推进）
// 则在 worktree 内把 main 合入 evolve 分支，再快进。
// 串行合并下逐个吸收前序任务，保证所有 released 任务最终都入 main，不卡死。
//
// 暂停语义（2026-10-25）：main 有未提交改动 → **暂停合并**（不写 pr_error，任务保持可自动补合），
// 之后每 MergeRetrySec（默认 300s = 5 分钟）重试检查一次 main 状态，干净了就把积压的任务合入。
// 暂停期间静默跳过（不刷日志/audit），仅首次进入暂缓态时记一条 audit。
func (d *driver) mergeToMain() {
	now := time.Now()
	if mergePaused(d.mergeDeferUntil.Load(), now) {
		return // 暂停中：等下一个 5 分钟检查周期
	}
	var tasks []db.EvolveTask
	if err := d.db.Where("status = ? AND (merge_commit IS NULL OR merge_commit = '') AND (pr_error IS NULL OR pr_error = '')", "released").Order("id").Find(&tasks).Error; err != nil {
		log.Printf("WARN: scan tasks for merge: %v", err)
		return
	}
	deferred := 0
	for i := range tasks {
		t := &tasks[i]
		commit, err := d.mergeTaskBranch(t)
		if err != nil {
			if errors.Is(err, errBaseWorktreeDirty) {
				// 相关（本次改动的）仓库有未提交改动 → 暂停合入（不写 pr_error，清理后自动补合）；看板显示阻塞
				deferred++
				d.mergeDeferUntil.Store(now.Add(time.Duration(d.cfg.MergeRetrySec) * time.Second).UnixNano())
				d.setMergeState(t.TaskNo, mergeStatePausedBaseDirty)
				if !d.mergeDeferAudited.Swap(true) {
					log.Printf("evolve: 相关仓库有未提交改动 → 暂停合入，每 %ds 重试检查（%v）", d.cfg.MergeRetrySec, err)
					d.store.Log(d.ctx(), "evolve-driver", "merge_paused_base_dirty", t.TaskNo, map[string]any{
						"retry_sec": d.cfg.MergeRetrySec, "reason": err.Error(),
					})
				}
				continue
			}
			log.Printf("WARN: merge %s: %v", t.TaskNo, err)
			// 合并冲突且开启了自动解决 → 保持可重试（下一轮再让 LLM 试），不写 pr_error。
			// 写 pr_error 意味着「需人工」并从合并循环里永久排除（旧版本就这么把任务卡死的）。
			if errors.Is(err, errMergeConflict) && d.cfg.AutoResolveConflicts {
				d.setMergeState(t.TaskNo, "conflict")
				d.store.Log(d.ctx(), "evolve-driver", "merge_conflict_retry", t.TaskNo, map[string]any{
					"error": tail(err.Error(), 300), "retry_sec": d.cfg.MergeRetrySec,
				})
				continue
			}
			d.db.Model(&db.EvolveTask{}).Where("task_no = ?", t.TaskNo).Update("pr_error", tail(err.Error(), 4000))
			d.setMergeState(t.TaskNo, "") // 转为需人工处理：不再是「暂停合入」
			d.store.Log(d.ctx(), "evolve-driver", "merge_failed", t.TaskNo, map[string]any{"error": err.Error()})
			continue
		}
		if err := d.db.Model(&db.EvolveTask{}).Where("task_no = ?", t.TaskNo).Update("merge_commit", commit).Error; err != nil {
			log.Printf("WARN: 回写 merge_commit %s: %v", t.TaskNo, err)
			continue
		}
		d.setMergeState(t.TaskNo, "") // 合入成功 → 解除阻塞显示
		d.store.Log(d.ctx(), "evolve-driver", "merged_main", t.TaskNo, map[string]any{
			"commit": commit, "branch": "evolve/" + t.TaskNo,
		})
		log.Printf("evolve %s: merged to local main (%s)", t.TaskNo, tail(commit, 8))
		d.cleanupWorktree(t.TaskNo, "merge-success") // 合并成功 → 立即清理 worktree 目录（含 git 注册）
	}
	if deferred == 0 {
		// 本轮无暂缓（或已恢复）：复位闸门，下次再脏时重新提示并重新进入 5 分钟重试
		d.mergeDeferUntil.Store(0)
		d.mergeDeferAudited.Store(false)
	}
}

// mergeTaskBranch 提交 worktree 改动（adapter 只产出 diff 不提交）→ 快进本地 main，返回合入的 sha。
// 多仓库：主仓库 + 每个子仓库各自 commit + 快进，返回主仓库 sha（子仓库 sha 仅留痕）。
//
// 全有或全无（2026-10-25）：先收集本任务**真正改了的**仓库（branchSHA ≠ baseSHA），
// 对它们逐一预检——任一相关仓库的 base 工作区有未提交改动 → **整体不合并**（连主仓库也不动，
// 避免出现「主仓库合了、子仓库没合」的半成品状态），等清理后下轮重试。
func (d *driver) mergeTaskBranch(t *db.EvolveTask) (string, error) {
	workdir := d.cfg.RepoDir + "/" + d.cfg.EvolveCacheDir + "/" + t.TaskNo
	if _, err := os.Stat(workdir); err != nil {
		return "", fmt.Errorf("worktree 不存在（%s），需手动合并", workdir)
	}
	// worktree 可能是**在宿主上**建的（.git 写死宿主绝对路径）→ 容器内 /repo 解析不了。
	// ensureWorktree 建树时会相对化，但手工/外部建的不会；合入前统一纠正（幂等）。
	d.relativizeWorktree(workdir)
	branch := "evolve/" + t.TaskNo

	// 1. 收集仓库列表：主仓库 + 建了 worktree 的子仓库（各自默认分支，与 ensureSubRepoWorktrees 同口径）
	repos := []mergeRepo{{name: "", gitRoot: d.cfg.RepoDir, workdir: workdir, base: d.cfg.BaseBranch}}
	for _, name := range d.subRepos() {
		subMain := d.cfg.RepoDir + "/apps/" + name
		subWT := workdir + "/apps/" + name
		if _, err := os.Stat(subWT + "/.git"); err != nil {
			continue // 该子仓库无 worktree（未建/无改动）
		}
		subBase := d.cfg.BaseBranch
		if out, err := runGit(subMain, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			if ref := strings.TrimSpace(out); ref != "" && ref != "HEAD" {
				subBase = ref
			}
		}
		repos = append(repos, mergeRepo{name: name, gitRoot: subMain, workdir: subWT, base: subBase})
	}

	// 2. 提交各 worktree 的 agent 改动（无改动则容忍）——未提交时不算「有改动」，必须先提交
	for _, r := range repos {
		if err := d.commitWorktree(r.workdir, t); err != nil {
			return "", err
		}
	}

	// 3. 预检：只针对本次真的改了的仓库（base..branch 有本任务提交）。
	//    判据不能用「branchSHA != baseSHA」：分支落后 base（base 已前进、本任务没改它）时
	//    SHA 也不同，但该仓库与本次合并无关——其工作区脏不脏都不能阻塞整单合并
	//    （2026-10-27 实测：只改 dapp 的任务被无关的 tee-node 工作区卡住）。
	relevant := make([]mergeRepo, 0, len(repos))
	for _, r := range repos {
		hasChanges, err := branchHasChangesToLand(r.gitRoot, branch, r.base)
		if err != nil {
			return "", fmt.Errorf("%s: %w", repoLabel(r.name), err)
		}
		if !hasChanges {
			continue // 本仓库无本任务提交（含分支落后 base）→ 不参与合并，也不检查其工作区
		}
		if wt := baseWorktreePath(r.gitRoot, r.base); wt != "" && baseWorktreeDirty(wt) {
			// 相关仓库有未提交改动 → 整体暂停（不合并任何仓库）
			return "", dirtyRepoError(r.name, wt)
		}
		relevant = append(relevant, r)
	}

	// 4. 逐个落地（预检已保证干净；主仓库在前，返回主仓库 sha）
	mainSHA := ""
	for _, r := range relevant {
		sha, err := d.mergeOneRepo(r.gitRoot, r.workdir, branch, r.base, t)
		if err != nil {
			return "", fmt.Errorf("%s: %w", repoLabel(r.name), err)
		}
		if r.name == "" {
			mainSHA = sha
		} else {
			d.store.Log(d.ctx(), "evolve-driver", "subrepo_merged", t.TaskNo, map[string]any{"repo": r.name, "commit": sha, "base": r.base})
		}
	}
	if mainSHA == "" {
		// 主仓库本次无改动（只改了子仓库）：返回主仓库 base 作为留痕 sha
		if sha, err := revParse(d.cfg.RepoDir, d.cfg.BaseBranch); err == nil {
			mainSHA = sha
		}
	}
	return mainSHA, nil
}

// mergeRepo 描述一个参与合并的仓库（主仓库 name="" / 子仓库 name=apps/<name>）。
type mergeRepo struct {
	name    string
	gitRoot string
	workdir string
	base    string
}

// branchHasChangesToLand 判断本任务的 evolve 分支在该仓库是否有需要落地的提交。
//
// 判据是 base..branch 的提交数 > 0，而不是 branchSHA != baseSHA：分支落后 base
// （base 已前进、本任务未改该仓库）时 SHA 同样不同，但该仓库与本次合并无关，
// 不能因它的工作区有未提交改动就暂停整单合并。
func branchHasChangesToLand(gitRoot, branch, base string) (bool, error) {
	out, err := runGit(gitRoot, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return false, fmt.Errorf("rev-list %s..%s: %v", base, branch, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return false, fmt.Errorf("rev-list %s..%s: 无法解析 %q: %v", base, branch, out, err)
	}
	return n > 0, nil
}

// repoLabel 日志/错误里标识仓库（主仓库返回「主仓库」）。
func repoLabel(name string) string {
	if name == "" {
		return "主仓库"
	}
	return "子仓库 " + name
}

// errBaseWorktreeDirty 表示（主/子）仓库检出 base 的工作区有未提交改动 → 本次合入暂缓（瞬时状态，
// 不写 pr_error，清理后自动重试）。
var errBaseWorktreeDirty = errors.New("仓库工作区有未提交改动")

// errMergeConflict 表示合入 base 时产生文本冲突。
// 开启自动解决（EVOLVE_AUTO_RESOLVE_CONFLICTS）时**不写 pr_error** —— 那是「需人工」的永久标记，
// 写死后任务再也不进合并循环（旧版本就是这样把 #123/#128 卡在 Review 的）；
// 冲突应保持可重试，交给下一轮自动解决。
var errMergeConflict = errors.New("合并冲突")

// dirtyRepoError 组装「某仓库工作区不干净 → 暂停合入」的错误：带上仓库名与路径，
// 便于看板/audit/日志定位到底是哪个相关仓库（name 为空 = 主仓库）。
func dirtyRepoError(name, path string) error {
	if name == "" {
		return fmt.Errorf("%w（主仓库：%s）；清理并提交后自动合入", errBaseWorktreeDirty, path)
	}
	return fmt.Errorf("%w（子仓库 %s：%s）；清理并提交后自动合入", errBaseWorktreeDirty, name, path)
}

// mergeStatePausedBaseDirty：evolve_tasks.merge_state 取值——相关仓库工作区有未提交改动 → 合入暂停，
// 看板把该任务显示为「阻塞：等待合入」，driver 每 EVOLVE_MERGE_RETRY_SEC 秒重试检查。
const mergeStatePausedBaseDirty = "paused_base_dirty"

// setMergeState 回写进化单的合入阻塞态（"" = 正常；mergeStatePausedBaseDirty = 暂停合入）。
func (d *driver) setMergeState(taskNo, state string) {
	if err := d.db.Model(&db.EvolveTask{}).Where("task_no = ?", taskNo).
		Update("merge_state", state).Error; err != nil {
		log.Printf("WARN: 回写 merge_state %s=%q: %v", taskNo, state, err)
	}
}

// mergeOneRepo 在单个 git 仓库内：提交 workdir 改动 → **先 pull base（把 base 合入分支，
// 冲突先解决再提交）** → 快进该仓库 base 分支，返回合入 sha。
// gitRoot 是仓库根（主仓库 = RepoDir，子仓库 = RepoDir/apps/<name>）；workdir 是该仓库的 worktree；
// base 是该仓库的默认分支（主仓库 = main，子仓库 = 各自 HEAD 指向分支）。
//
// 落地协议（2026-10-24 明确）：
//  1. **落地前必须先 pull base**：把 base 合入分支，有冲突先解决再提交（绝不静默取舍——
//     旧实现 `merge -X ours` 会在冲突时无声丢弃 main 侧改动）；
//  2. **base 的工作区必须干净才能落地**：不干净 → 本次「暂停合入」（什么都不做，
//     等 mergeToMain 的下一个重试周期；默认 5 分钟）。否则 ref 前进了而工作区停在旧提交：
//     之后在该工作区里的任何提交都会把本次改动「静默回退」（2026-10-14~10-23 实测丢失 4 次）；
//  3. 落地后把该工作区同步到新 base（reset --hard），保证工作区永不停在旧提交。
func (d *driver) mergeOneRepo(gitRoot, workdir, branch, base string, t *db.EvolveTask) (string, error) {
	// 1. 提交 coder 改动（无改动则容忍）
	if err := d.commitWorktree(workdir, t); err != nil {
		return "", err
	}

	// 2. 快进检查：base 必须是分支祖先
	branchSHA, err := revParse(gitRoot, branch)
	if err != nil {
		return "", fmt.Errorf("rev-parse %s: %v", branch, err)
	}
	baseSHA, err := revParse(gitRoot, base)
	if err != nil {
		return "", fmt.Errorf("rev-parse %s: %v", base, err)
	}
	if branchSHA == baseSHA {
		return baseSHA, nil // 本仓库无新提交（空 diff）→ 无需合并，base 脏不脏与本次无关
	}

	// 3. 落地前守卫：确实有东西要落地时，base 工作区必须干净（否则暂停合入，不做任何提交/推进）
	baseWT := baseWorktreePath(gitRoot, base)
	if baseWT != "" && baseWorktreeDirty(baseWT) {
		return "", fmt.Errorf("%w（%s）；清理并提交后自动合入", errBaseWorktreeDirty, baseWT)
	}

	// 4. 先 pull base：把 base 合入 evolve 分支（不碰 base 工作区）。
	//    合并后分支成为 base 后代 → 可快进。串行合并下逐个吸收前序任务，最终全部入 base。
	merged, err := d.syncBranchWithBase(gitRoot, workdir, branch, base)
	if err != nil {
		return "", err
	}
	if merged {
		if branchSHA, err = revParse(gitRoot, branch); err != nil {
			return "", fmt.Errorf("rev-parse %s (post-merge): %v", branch, err)
		}
	}

	// 5. 落地前二次确认：期间 base 工作区不能被写脏（否则宁可暂缓，也不冒「静默回退」风险）
	if baseWT != "" && baseWorktreeDirty(baseWT) {
		return "", fmt.Errorf("%w（%s）；清理并提交后自动合入", errBaseWorktreeDirty, baseWT)
	}

	// 6. 原子 CAS 快进 base
	if out, err := runGit(gitRoot, "update-ref", "refs/heads/"+base, branchSHA, baseSHA); err != nil {
		return "", fmt.Errorf("update-ref %s: %v: %s", base, err, tail(out, 200))
	}

	// 6. 同步该工作区到新 base（验收过的干净前提下等同 checkout；失败只告警 + audit，提示人工 pull）
	if baseWT != "" {
		if out, err := runGit(baseWT, "reset", "--hard", branchSHA); err != nil {
			log.Printf("WARN: %s 工作区同步失败（%s）：请人工 git reset --hard %s：%v: %s", base, baseWT, tail(branchSHA, 8), err, tail(out, 200))
			d.store.Log(d.ctx(), "evolve-driver", "base_worktree_sync_failed", t.TaskNo, map[string]any{
				"base": base, "worktree": baseWT, "commit": branchSHA, "error": tail(out, 200),
			})
		} else {
			d.store.Log(d.ctx(), "evolve-driver", "base_worktree_synced", t.TaskNo, map[string]any{
				"base": base, "worktree": baseWT, "commit": branchSHA,
			})
		}
	}
	return branchSHA, nil
}

// commitWorktree 提交 worktree 内的 agent 改动（无改动则容忍）。
func (d *driver) commitWorktree(workdir string, t *db.EvolveTask) error {
	if out, err := runGit(workdir, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %v: %s", err, tail(out, 200))
	}
	if out, err := runGit(workdir,
		"-c", "user.name=evolve-driver",
		"-c", "user.email=evolve-driver@computeflux.local",
		"commit", "-m", mergeCommitMsg(t)); err != nil {
		if !strings.Contains(out, "nothing to commit") && !strings.Contains(out, "no changes added") {
			return fmt.Errorf("git commit: %v: %s", err, tail(out, 200))
		}
	}
	return nil
}

// syncBranchWithBase 落地/推送前先「pull base」：把 base 合入 workdir 所在的分支。
// 返回 true 表示产生了合并提交（分支 SHA 变化）。
// 冲突 → **中止合并并返回错误**（先解决冲突再提交，绝不静默取舍）。
func (d *driver) syncBranchWithBase(gitRoot, workdir, branch, base string) (bool, error) {
	baseSHA, err := revParse(gitRoot, base)
	if err != nil {
		return false, fmt.Errorf("rev-parse %s: %v", base, err)
	}
	branchSHA, err := revParse(gitRoot, branch)
	if err != nil {
		return false, fmt.Errorf("rev-parse %s: %v", branch, err)
	}
	if _, err := runGit(gitRoot, "merge-base", "--is-ancestor", baseSHA, branchSHA); err == nil {
		return false, nil // base 已在分支内：无可合并内容
	}
	if out, err := runGit(workdir,
		"-c", "user.name=evolve-driver",
		"-c", "user.email=evolve-driver@computeflux.local",
		"merge", "--no-edit", base); err != nil {
		conflictsOut, _ := runGit(workdir, "diff", "--name-only", "--diff-filter=U")
		files := strings.Fields(conflictsOut)
		filesStr := strings.Join(files, " ")

		// 先尝试自动解决（LLM 逐文件三方合并）——成功则提交合并提交、流程继续。
		// 解决方向是「base 合入 evolve 分支」，主线尚未推进；失败一律 abort 交人工。
		if resolved, rerr := d.autoResolveMergeConflicts(d.ctx(), workdir, files); rerr == nil {
			if aout, aerr := runGit(workdir, append([]string{"add", "--"}, resolved...)...); aerr != nil {
				d.store.Log(d.ctx(), "evolve-driver", "merge_conflict_auto_resolve_failed", tail(branch, 80), map[string]any{
					"base": base, "stage": "git add", "error": tail(aout, 200),
				})
			} else if cout, cerr := runGit(workdir,
				"-c", "user.name=evolve-driver",
				"-c", "user.email=evolve-driver@computeflux.local",
				"commit", "--no-edit"); cerr != nil {
				d.store.Log(d.ctx(), "evolve-driver", "merge_conflict_auto_resolve_failed", tail(branch, 80), map[string]any{
					"base": base, "stage": "git commit", "error": tail(cout, 200),
				})
			} else {
				d.store.Log(d.ctx(), "evolve-driver", "merge_conflict_auto_resolved", tail(branch, 80),
					map[string]any{"base": base, "conflict_files": files, "resolved_files": resolved})
				log.Printf("evolve: 自动解决合并冲突（%s，%d 个文件）：%s", base, len(resolved), strings.Join(resolved, ", "))
				return true, nil
			}
		} else {
			d.store.Log(d.ctx(), "evolve-driver", "merge_conflict_auto_resolve_failed", tail(branch, 80), map[string]any{
				"base": base, "files": filesStr, "error": tail(rerr.Error(), 300),
			})
			log.Printf("evolve: 自动解决失败（%s）：%v → 交人工", base, rerr)
		}

		// 兜底：中止合并，保持原有「交人工」行为
		runGit(workdir, "merge", "--abort")
		d.store.Log(d.ctx(), "evolve-driver", "merge_conflict", tail(branch, 80), map[string]any{
			"base": base, "files": filesStr,
		})
		return false, fmt.Errorf("%w：与 %s 合并冲突：先解决冲突再提交（冲突文件：%s）: %s", errMergeConflict, base, filesStr, tail(out, 200))
	}
	return true, nil
}

// revParse 解析 ref → sha。
func revParse(dir, ref string) (string, error) {
	out, err := runGit(dir, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, tail(out, 200))
	}
	return strings.TrimSpace(out), nil
}

// baseWorktreePath 返回检出 base 分支的 worktree 路径（无 / 已不存在则空串）。
// 跳过不存在的路径：注册了但已被 prune / 不在本容器可见范围（如宿主路径）的工作区无需守卫。
func baseWorktreePath(gitRoot, base string) string {
	out, err := runGit(gitRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	wt := parseWorktreeForBranch(out, base)
	if wt == "" {
		return ""
	}
	if _, err := os.Stat(wt); err != nil {
		return ""
	}
	return wt
}

// parseWorktreeForBranch 从 `git worktree list --porcelain` 输出里找出检出 base 的 worktree 路径。
func parseWorktreeForBranch(porcelain, base string) string {
	path := ""
	for _, line := range strings.Split(porcelain, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch "):
			if strings.TrimSpace(strings.TrimPrefix(line, "branch ")) == "refs/heads/"+base {
				return path
			}
		case strings.TrimSpace(line) == "":
			path = "" // 块结束（防止 detached 块继承上一块路径）
		}
	}
	return ""
}

// baseWorktreeDirty 判断工作区是否有未提交（含暂存）改动；未跟踪文件不算（不影响 reset --hard）。
func baseWorktreeDirty(wt string) bool {
	out, err := runGit(wt, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return true // 探不到状态时按「不干净」处理：宁可暂缓合入，也不制造幻影差异
	}
	return strings.TrimSpace(out) != ""
}

// mergeCommitMsg 生成合并提交信息（进化单号 + 任务首行）。
func mergeCommitMsg(t *db.EvolveTask) string {
	ev := parseEvidence(t.Evidence)
	taskText, _ := ev["task"].(string)
	return fmt.Sprintf("evolve(%s): %s", t.TaskNo, firstLine(taskText, 100))
}

// runGit 在指定目录跑 git 命令，返回合并输出。
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runTask 执行进化链（§3.8）。所有状态迁移写 audit_logs。
