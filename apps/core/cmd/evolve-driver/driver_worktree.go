package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wetee/ai-workflow/internal/agentcli"
	"github.com/wetee/ai-workflow/internal/db"
)

// ctx 简短 helper。
func (d *driver) ctx() context.Context { return context.Background() }

// subRepos 返回 apps/ 下独立 git 仓库（子仓库）的目录名列表。
// 判定：apps/<name>/.git 存在（独立仓库，被主仓库 .gitignore 忽略）。
// 这类子仓库不在主仓库 worktree 里（git 不 checkout gitignored 目录），
// 需单独建嵌套 worktree 才能被 coder 看到/编辑（多仓库支持，2026-10-16）。
func (d *driver) subRepos() []string {
	entries, err := os.ReadDir(d.cfg.RepoDir + "/apps")
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(d.cfg.RepoDir + "/apps/" + e.Name() + "/.git"); err == nil {
			repos = append(repos, e.Name())
		}
	}
	return repos
}

// ensureSubRepoWorktrees 为每个独立子仓库建/复用嵌套 worktree：workdir/apps/<name>，分支 evolve/<task_no>。
// 子仓库的 git 根在 <repo>/apps/<name>，worktree 落在主 worktree 的 apps/<name>（主仓库 gitignored，互不干扰）。
// 单个子仓库失败只告警不阻断（主仓库 worktree 仍可用，coder 至少能改主仓库部分）。
func (d *driver) ensureSubRepoWorktrees(task *db.EvolveTask, workdir string) {
	branch := "evolve/" + task.TaskNo
	for _, name := range d.subRepos() {
		subMain := d.cfg.RepoDir + "/apps/" + name
		subWT := workdir + "/apps/" + name
		// 已存在（.git 文件/目录）→ 复用
		if _, err := os.Stat(subWT + "/.git"); err == nil {
			d.relativizeWorktree(subWT) // 旧 worktree 的 gitdir 可能是容器绝对路径 → 修回相对
			d.lockWorktree(subMain, subWT)
			continue
		}
		// 子仓库默认分支（HEAD 指向的）：不硬编码 main（部分子仓库默认分支是 master 等）
		baseRef := d.cfg.BaseBranch
		if out, err := runGit(subMain, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			if ref := strings.TrimSpace(out); ref != "" && ref != "HEAD" {
				baseRef = ref
			}
		}
		if out, err := runGit(subMain, "worktree", "add", subWT, "-b", branch, baseRef); err != nil {
			// 分支已存在（上次残留）→ 不带 -b 复用
			if strings.Contains(string(out), "already exists") {
				if out2, err2 := runGit(subMain, "worktree", "add", subWT, branch); err2 != nil {
					log.Printf("WARN: sub-repo worktree %s: %v: %s", name, err2, out2)
					continue
				}
			} else {
				log.Printf("WARN: sub-repo worktree %s: %v: %s", name, err, out)
				continue
			}
		}
		d.relativizeWorktree(subWT) // 相对化：容器内 /repo 与宿主路径均可解析
		d.lockWorktree(subMain, subWT)
		log.Printf("evolve: sub-repo worktree created %s → %s (base=%s)", name, subWT, baseRef)
	}
}

// ensureWorktree 建/复用 git worktree：<repo>/<cache>/<task_no>，分支 evolve/<task_no>。
// 主仓库 worktree + 每个独立子仓库的嵌套 worktree（多仓库支持）。
func (d *driver) ensureWorktree(task *db.EvolveTask) (string, error) {
	workdir := d.cfg.RepoDir + "/" + d.cfg.EvolveCacheDir + "/" + task.TaskNo
	branch := "evolve/" + task.TaskNo

	// 已存在（重试场景）→ 复用（修正 gitdir 为相对路径；并补同步缺失的 .venv——
	// 首次建树时宿主可能还没有对应 .venv，沙箱/CI 会因缺 python 树 exit 127）
	if _, err := os.Stat(workdir); err == nil {
		d.relativizeWorktree(workdir)
		d.lockWorktree(d.cfg.RepoDir, workdir)
		if err := d.syncVenvs(workdir); err != nil {
			log.Printf("WARN: sync .venv (reuse): %v（沙箱 python 单测可能失败）", err)
		}
		d.ensureSubRepoWorktrees(task, workdir)
		return workdir, nil
	}

	cmd := exec.Command("git", "worktree", "add", workdir, "-b", branch, d.cfg.BaseBranch)
	cmd.Dir = d.cfg.RepoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 分支已存在（上次失败后分支残留）→ 不带 -b 复用
		if strings.Contains(string(out), "already exists") {
			cmd2 := exec.Command("git", "worktree", "add", workdir, branch)
			cmd2.Dir = d.cfg.RepoDir
			out2, err2 := cmd2.CombinedOutput()
			if err2 != nil {
				return "", fmt.Errorf("git worktree add: %v: %s (reuse: %s)", err, out, out2)
			}
		} else {
			return "", fmt.Errorf("git worktree add: %v: %s", err, out)
		}
	}

	// .venv 不进 git，沙箱/CI 离线跑 `.venv/bin/python -m pytest` 需要它 → 从宿主仓库拷进 worktree
	if err := d.syncVenvs(workdir); err != nil {
		log.Printf("WARN: sync .venv: %v（沙箱 python 单测可能失败）", err)
	}
	d.relativizeWorktree(workdir)
	d.lockWorktree(d.cfg.RepoDir, workdir)
	d.ensureSubRepoWorktrees(task, workdir)
	return workdir, nil
}

// worktreeHealthy 验证 worktree（主 + 已建嵌套子仓库）的 git 可用性：`.git` 指针存在
// （目录或 gitdir 文件）且 git status 真的跑得通。防「目录被外部重建/替换后 .git 指针失效」——
// 此时所有变更检测永远为空，任务会连烧 MaxRounds 轮 empty-diff（2026-11-10 WI-1794313637 实测）。
func (d *driver) worktreeHealthy(workdir string) bool {
	if !worktreeGitOK(workdir) {
		return false
	}
	for _, name := range d.subRepos() {
		subWT := workdir + "/apps/" + name
		if _, err := os.Stat(subWT); os.IsNotExist(err) {
			continue // 该子仓库未建树：不强制（与 ensureSubRepoWorktrees 容错口径一致）
		}
		if !worktreeGitOK(subWT) {
			log.Printf("WARN: sub worktree %s git 不可用", subWT)
			return false
		}
	}
	return true
}

// worktreeGitOK 单个工作区的 git 可用性探测。
func worktreeGitOK(workdir string) bool {
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err != nil {
		return false
	}
	out, err := runGit(workdir, "status", "--porcelain")
	if err != nil {
		log.Printf("WARN: worktree %s git status 失败: %v: %s", workdir, err, tail(out, 200))
		return false
	}
	return true
}

// unhealthySubWorktrees 列出目录存在但 git 不可用的子仓库名（典型：被手工替换成普通拷贝，
// .git 指针丢失 → CollectChanges / HasChanges 对它永远采不到变更）。
func (d *driver) unhealthySubWorktrees(workdir string) []string {
	var bad []string
	for _, name := range d.subRepos() {
		subWT := workdir + "/apps/" + name
		if _, err := os.Stat(subWT); err != nil {
			continue // 未建树 → ensureSubRepoWorktrees 会补建
		}
		if !worktreeGitOK(subWT) {
			bad = append(bad, name)
		}
	}
	return bad
}

// rebuildSubWorktrees 拆除并重建指定子仓库的嵌套 worktree（只动这些，主目录与其它子仓库不碰）。
func (d *driver) rebuildSubWorktrees(task *db.EvolveTask, workdir string, names []string) {
	for _, name := range names {
		subMain := d.cfg.RepoDir + "/apps/" + name
		subWT := workdir + "/apps/" + name
		runGit(subMain, "worktree", "unlock", subWT)
		if out, err := runGit(subMain, "worktree", "remove", "--force", subWT); err != nil {
			log.Printf("WARN: sub-repo worktree remove %s: %v: %s（兜底删目录）", name, err, tail(out, 200))
			_ = os.RemoveAll(subWT)
			runGit(subMain, "worktree", "prune")
		}
		log.Printf("evolve: sub-repo worktree removed %s (rebuild)", name)
	}
	d.ensureSubRepoWorktrees(task, workdir)
}

// writesOutsideWorkdir 会话里 edit/write 目标路径中落在 workdir 之外的部分（最多 5 条）。
// 无法判定的条目不计入（保守：只在确凿越界时触发 fail-fast）。
func (d *driver) writesOutsideWorkdir(task *db.EvolveTask, workdir, sessionID string) []string {
	targets := d.sessionWriteTargets(sessionID)
	if len(targets) == 0 {
		return nil
	}
	// 同根比较：workdir 是容器路径（/repo/...），会话里是 pi-web 看到的 cwd——两侧都先
	// EvalSymlinks 后按真实路径比，符号链接挂载场景不会误报。
	base := workdir
	if r, err := filepath.EvalSymlinks(workdir); err == nil {
		base = r
	}
	var outside []string
	for _, p := range targets {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workdir, p)
		}
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r
		} else if !strings.Contains(p, "..") {
			// 文件不存在（新建文件）→ EvalSymlinks 对「最长存在的父目录」求值再拼回尾巴，
			// 保证 /repo 前缀的符号链接场景也能判定
			if r2, err2 := filepath.EvalSymlinks(filepath.Dir(abs)); err2 == nil {
				abs = filepath.Join(r2, filepath.Base(abs))
			} else {
				continue // 完全无法解析 → 不判
			}
		}
		if rel, err := filepath.Rel(base, abs); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			outside = append(outside, p)
		}
		if len(outside) >= 5 {
			break
		}
	}
	return outside
}

// sessionWriteTargets 从 pi-web 会话文件提取本轮 coder 实际 edit/write 的目标路径清单
// （诊断用：子仓库改动在 worktree 重建后丢失，audit 是唯一留痕）。
// 任何失败返回 nil（尽力而为，绝不阻断进化链）。
func (d *driver) sessionWriteTargets(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	file := agentcli.SessionFileByID(agentcli.LoadPiEnv().SessionDir, sessionID)
	if file == "" {
		return nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "toolCall") {
			continue
		}
		var entry struct {
			Message *struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string         `json:"type"`
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Message == nil {
			continue
		}
		if entry.Message.Role != "assistant" {
			continue
		}
		for _, c := range entry.Message.Content {
			if c.Type != "toolCall" || (c.Name != "edit" && c.Name != "write") {
				continue
			}
			if p, _ := c.Arguments["path"].(string); p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// resetWorktree 把 worktree 重置到分支 HEAD 的干净状态（丢弃未提交变更 + 删 untracked 文件）。
// 任务重启（打回重跑 / 启动恢复）时调用：上次运行残留的未提交代码会让 agent 误判"已完成"→ empty diff。
// 只影响工作区，不碰分支历史（git checkout + clean，不 reset --hard 到别的 commit）。
// 多仓库：主仓库 + 每个子仓库各自重置。
func (d *driver) resetWorktree(workdir string) {
	d.resetOneWorktree(workdir)
	// 子仓库各自重置（嵌套 worktree 在 workdir/apps/<name>）
	for _, name := range d.subRepos() {
		subWT := workdir + "/apps/" + name
		if _, err := os.Stat(subWT + "/.git"); err == nil {
			d.resetOneWorktree(subWT)
		}
	}
	log.Printf("evolve: worktree reset to clean state (%s)", workdir)
}

// resetOneWorktree 重置单个 git 工作区（主仓库或子仓库）。
func (d *driver) resetOneWorktree(workdir string) {
	// 丢弃已跟踪文件的未提交变更
	cmd1 := exec.Command("git", "checkout", "--", ".")
	cmd1.Dir = workdir
	if out, err := cmd1.CombinedOutput(); err != nil {
		log.Printf("WARN: resetWorktree checkout: %v: %s", err, out)
	}
	// 删 untracked 文件/目录（保留 .venv——沙箱/CI 需要）
	cmd2 := exec.Command("git", "clean", "-fd", "-e", ".venv", "-e", "**/.venv")
	cmd2.Dir = workdir
	if out, err := cmd2.CombinedOutput(); err != nil {
		log.Printf("WARN: resetWorktree clean: %v: %s", err, out)
	}
}

// relativizeWorktree 把 worktree 侧的 .git 指针改成相对路径（跨挂载点可用：driver /repo、pi-web /repo+/work、宿主）。
// 必要性：`git worktree add` 喉绝对路径，同一 worktree 被不同挂载点的容器共用时会失效
// （2026-09-30 实测：变更检测永远为空、写范围校验形同虚设）。
// ⚠️ admin 侧 .git/worktrees/<name>/gitdir 不改：git 校验要求它是**绝对路径**，
// 相对路径会让 git worktree list 判 prunable、prune 时当陈旧项删掉（2026-10-17 实测：
// 误删了 4 个含未提交产出的 worktree 注册 → 靠重建 admin + read-tree 修复）。
// 该文件只在「主仓库侧 worktree 管理」用到（list/prune），pipeline 本身靠 worktree 侧 .git → admin → commondir 解析。
// 配套防护：建树后调 lockWorktree（prune 跳过 locked）。
func (d *driver) relativizeWorktree(workdir string) {
	wtGit := filepath.Join(workdir, ".git")
	b, err := os.ReadFile(wtGit)
	if err != nil {
		return // .git 是目录（非 worktree）或无权限
	}
	const p = "gitdir: "
	s := strings.TrimSpace(string(b))
	if !strings.HasPrefix(s, p) {
		return
	}
	adminDir := strings.TrimSpace(strings.TrimPrefix(s, p))
	if !filepath.IsAbs(adminDir) {
		return // 已是相对路径（重试场景）
	}
	if rel, err := filepath.Rel(workdir, adminDir); err == nil {
		if err := os.WriteFile(wtGit, []byte(p+rel+"\n"), 0o644); err != nil {
			log.Printf("WARN: relativize worktree .git: %v", err)
		}
	}
}

// lockWorktree 给 worktree 加 lock（幂等）：prune 会跳过 locked，防宿主侧/误操作 prune
// 把容器内的注册当陈旧项清掉（2026-10-17 踩坑）。gitRoot = 仓库根（主仓库或子仓库）。
func (d *driver) lockWorktree(gitRoot, workdir string) {
	out, err := runGit(gitRoot, "worktree", "lock", "--reason", "evolve-driver managed (do not prune)", workdir)
	if err != nil && !strings.Contains(out, "already locked") {
		log.Printf("WARN: lock worktree %s: %v: %s", workdir, err, tail(out, 200))
	}
}

// syncVenvs 把宿主仓库 apps/*/.venv 拷进 worktree（幂等：已存在则跳过）。
// 约定：宿主 venv 由 embed_tree 预处理——uv python 树内嵌在 .venv/python（自包含，
// bin/python* 为相对链接），cp -a 后容器内即可运行；embedVenvPython 做校验
// 并把树自带 site-packages 换成指向 venv site-packages 的相对链接。
// 沙箱/CI 用 `PYTHONHOME=.venv/python .venv/python/bin/python -m pytest` 运行。
func (d *driver) syncVenvs(workdir string) error {
	entries, err := os.ReadDir(d.cfg.RepoDir + "/apps")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := d.cfg.RepoDir + "/apps/" + e.Name() + "/.venv"
		dst := workdir + "/apps/" + e.Name() + "/.venv"
		if _, err := os.Stat(src); err != nil {
			continue // 宿主无 .venv（该应用无 python 依赖）
		}
		if _, err := os.Stat(dst); err == nil {
			continue // worktree 已有（重试场景）
		}
		if out, err := exec.Command("cp", "-a", src, dst).CombinedOutput(); err != nil {
			return fmt.Errorf("cp %s: %v: %s", src, err, out)
		}
		if err := d.embedVenvPython(dst); err != nil {
			return fmt.Errorf("embed %s: %w", dst, err)
		}
	}
	return nil
}

// embedVenvPython：校验 .venv/python（宿主预处理内嵌的 uv python 树）随 cp -a 完整拷入，
// 并把树自带的 lib/python3.x/site-packages（仅 pip）替换为指向 venv site-packages 的
// 相对链接（容器内解析到 worktree 内，只读挂载可用）。
func (d *driver) embedVenvPython(venv string) error {
	if _, err := os.Stat(venv + "/python/bin/python3.13"); err != nil {
		return fmt.Errorf(".venv/python 缺失或不可用（宿主 venv 未内嵌 uv python 树）: %v", err)
	}
	libs, err := os.ReadDir(venv + "/python/lib")
	if err != nil {
		return err
	}
	for _, l := range libs {
		if !strings.HasPrefix(l.Name(), "python3") {
			continue
		}
		sp := venv + "/python/lib/" + l.Name() + "/site-packages"
		if _, err := os.Lstat(sp); err == nil {
			if err := os.RemoveAll(sp); err != nil {
				return err
			}
		}
		if err := os.Symlink(filepath.Join("..", "..", "..", "lib", l.Name(), "site-packages"), sp); err != nil {
			return err
		}
	}
	return nil
}

// runAgentHost 在 evolve-driver 容器内直接执行 adapter（宿主工具链经 /host 挂载）。
//
// adapter = 原生二进制（agent-pi / agent-ocr，Go 实现，见 internal/agentcli）：直接 exec。
//
// 替代旧的 docker.Exec 进 agent 容器模式：pi/ocr 等 CLI 经宿主 npm 全局安装，
// 容器只需 node + git，新装任何宿主工具自动可用。
