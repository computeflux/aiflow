package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Manager 管理一个主仓库下的 evolve worktree 集合。
type worktreeManager struct {
	RepoDir  string // 主仓库根（git 根）
	CacheDir string // worktree 缓存目录（绝对路径，如 /repo/cache/evolve）
	Base     string // 主仓库 base 分支（默认 main）
}

// New 构造 Manager。cacheDir 为绝对路径。
func newWorktreeManager(repoDir, cacheDir, base string) *worktreeManager {
	return &worktreeManager{RepoDir: repoDir, CacheDir: cacheDir, Base: base}
}

// Dir 返回任务对应的 worktree 目录。
func (m *worktreeManager) Dir(taskNo string) string {
	return m.CacheDir + "/" + taskNo
}

// Branch 返回任务对应的 evolve 分支名。
func (m *worktreeManager) Branch(taskNo string) string { return "evolve/" + taskNo }

// runGit 在指定目录跑 git 命令，返回合并输出。
func wtRunGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// BranchMerged 判断分支是否已合并进 base（merge-base --is-ancestor）。
// 分支或 base 不存在 → false（无法确认已合并 → 不清理）。
// 注意：只能回答「分支包含于 base」，无法区分「真合并过」与「从未提交过任何改动」
// （后者分支仍 = 创建时的 base commit）→ 调用方须配合 HasUncommitted 保守校验。
func (m *worktreeManager) BranchMerged(branch, base string) bool {
	if _, err := wtRunGit(m.RepoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		return false
	}
	if _, err := wtRunGit(m.RepoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+base); err != nil {
		return false
	}
	_, err := wtRunGit(m.RepoDir, "merge-base", "--is-ancestor", "refs/heads/"+branch, "refs/heads/"+base)
	return err == nil
}

// subRepoBase 返回子仓库（RepoDir/apps/<name>）当前 HEAD 指向的分支名（与
// evolve-driver ensureSubRepoWorktrees / mergeTaskBranch 同口径）；不是 git 仓库或
// detached HEAD 时返回空串。
func (m *worktreeManager) subRepoBase(name string) string {
	root := m.RepoDir + "/apps/" + name
	out, err := wtRunGit(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(out)
	if ref == "" || ref == "HEAD" {
		return ""
	}
	return ref
}

// subRepoMerged 判断子仓库的嵌套 worktree 分支是否已合并进子仓库 base。
// 子仓库无嵌套 worktree（目录不存在）→ true（无需等待）；
// 子仓库不是 git 仓库 / 分支不存在 / 未合并 → false（保守：不清理）。
func (m *worktreeManager) subRepoMerged(taskNo, name string) bool {
	subWT := m.Dir(taskNo) + "/apps/" + name
	if _, err := os.Stat(subWT + "/.git"); err != nil {
		return true // 无嵌套 worktree
	}
	root := m.RepoDir + "/apps/" + name
	if _, err := os.Stat(root + "/.git"); err != nil {
		return false // 子仓库本体不在（异常态）→ 保守不清理
	}
	base := m.subRepoBase(name)
	if base == "" {
		return false
	}
	branch := "evolve/" + taskNo
	if _, err := wtRunGit(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		return false
	}
	_, err := wtRunGit(root, "merge-base", "--is-ancestor", "refs/heads/"+branch, "refs/heads/"+base)
	return err == nil
}

// Remove 删除任务的 worktree：先解锁（evolve-driver 建树时 lockWorktree 防 prune，
// 清理必须显式解锁，否则 remove 失败后 prune 也跳过 locked 注册 → 永久残留），
// 再删嵌套子仓库 worktree（git 注册），再删主 worktree（git worktree remove --force，
// --force 越过脏工作区——已合并内容无保留价值），最后兜底删残留目录 + prune 陈旧注册。
// 幂等：目录不存在时直接返回 nil。
func (m *worktreeManager) Remove(taskNo string) error {
	dir := m.Dir(taskNo)
	if _, err := os.Stat(dir); err != nil {
		return nil // 已不存在（幂等）
	}
	// 0. 解锁主 worktree（未 lock 时忽略错误）
	wtRunGit(m.RepoDir, "worktree", "unlock", dir)
	// 1. 嵌套子仓库 worktree（各自 git 根在 RepoDir/apps/<name>）
	for _, name := range m.SubRepos() {
		subWT := dir + "/apps/" + name
		if _, err := os.Stat(subWT + "/.git"); err != nil {
			continue
		}
		root := m.RepoDir + "/apps/" + name
		// 子 worktree 建树时同样被 lockWorktree 加锁（ensureSubRepoWorktrees）→ 先解锁，
		// 否则 remove 恒失败、prune 又跳过 locked 注册 → 子仓库 .git/worktrees/* 永久残留
		wtRunGit(root, "worktree", "unlock", subWT)
		if out, err := wtRunGit(root, "worktree", "remove", "--force", subWT); err != nil {
			// git 注册删不掉（如 gitdir 指针失效）→ 兜底直接删目录 + prune，避免残留
			if err2 := os.RemoveAll(subWT); err2 != nil {
				return fmt.Errorf("remove sub worktree %s: %v: %s（兜底删目录: %v）", subWT, err, wtTail(out, 200), err2)
			}
			wtRunGit(root, "worktree", "prune")
		}
	}
	// 2. 主 worktree
	if out, err := wtRunGit(m.RepoDir, "worktree", "remove", "--force", dir); err != nil {
		if err2 := os.RemoveAll(dir); err2 != nil {
			return fmt.Errorf("remove worktree %s: %v: %s（兜底删目录: %v）", dir, err, wtTail(out, 200), err2)
		}
		// 目录已删但 git 注册可能残留（已解锁 → prune 可清）
		wtRunGit(m.RepoDir, "worktree", "prune")
	}
	// 3. 兜底：git remove 成功但目录残留（异常态）→ 直接删
	if _, err := os.Stat(dir); err == nil {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove dir %s: %v", dir, err)
		}
		wtRunGit(m.RepoDir, "worktree", "prune")
	}
	return nil
}

// SubRepos 返回 apps/ 下独立 git 仓库（子仓库）的目录名列表（与 evolve-driver 同口径：
// apps/<name>/.git 存在）。
func (m *worktreeManager) SubRepos() []string {
	entries, err := os.ReadDir(m.RepoDir + "/apps")
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(m.RepoDir + "/apps/" + e.Name() + "/.git"); err == nil {
			repos = append(repos, e.Name())
		}
	}
	return repos
}

// dirtyTracked 判断工作区是否有未提交（含暂存）的已跟踪文件改动；未跟踪文件不算
// （.venv 等宿主副本不影响）。探不到状态时按「有改动」处理（保守：宁可不清理）。
func dirtyTracked(dir string) bool {
	out, err := wtRunGit(dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return true
	}
	return strings.TrimSpace(out) != ""
}

// HasUncommitted 判断任务 worktree（主 + 嵌套子仓库）是否有未提交的已跟踪文件改动。
// 分支包含于 base ≠ 工作区内容已合并：从未提交过的任务（cancelled/打回）分支天然是 base
// 祖先，但目录里还有未提交 diff（且 rerun「续接执行」依赖该目录）→ 有改动即不可清理。
func (m *worktreeManager) HasUncommitted(taskNo string) bool {
	if dirtyTracked(m.Dir(taskNo)) {
		return true
	}
	for _, sub := range m.SubRepos() {
		subWT := m.Dir(taskNo) + "/apps/" + sub
		if _, err := os.Stat(subWT + "/.git"); err == nil && dirtyTracked(subWT) {
			return true
		}
	}
	return false
}

// Scan 扫描缓存目录，返回可安全清理的（taskNo, 目录）列表：
//   - 目录名在 active 集合中（进行中任务）→ 跳过（绝不误删）；
//   - 非 worktree 目录（无 .git 指针，如 *.review.json 之外的杂项）→ 跳过；
//   - 主仓库分支未合并进 base → 跳过；
//   - 主/子 worktree 有未提交改动 → 跳过（cancelled/打回任务：分支=base 祖先但目录有未提交 diff）；
//   - 任一子仓库嵌套 worktree 分支未合并 → 跳过。
//
// active 由调用方从 evolve_tasks 查得（运行态 + 审批中 + 已上线未合并等）。
func (m *worktreeManager) Scan(active map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(m.CacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 缓存目录不存在 → 无可清理
		}
		return nil, err
	}
	var stale []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if active[name] {
			continue // 进行中任务：绝不误删
		}
		dir := m.CacheDir + "/" + name
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue // 非 worktree 目录（无 .git 指针）→ 不碰
		}
		if !m.BranchMerged(m.Branch(name), m.Base) {
			continue // 未合并 → 保留
		}
		if m.HasUncommitted(name) {
			continue // 有未提交改动 → 保留（分支=base 祖先的 cancelled/打回任务靠这条兜住）
		}
		merged := true
		for _, sub := range m.SubRepos() {
			if !m.subRepoMerged(name, sub) {
				merged = false
				break
			}
		}
		if !merged {
			continue
		}
		stale = append(stale, name)
	}
	return stale, nil
}

func wtTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
