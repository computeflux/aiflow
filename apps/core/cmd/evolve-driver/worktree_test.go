package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCmd 在 dir 跑命令，失败即 t.Fatal（测试辅助）。
func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// gitc 以固定身份跑 git commit 类命令（测试仓库无全局 git 身份）。
func gitc(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)
	runCmd(t, dir, "git", full...)
}

// setupRepo 建一个临时主仓库（含 apps/sub 子仓库），返回 (repoDir, cacheDir)。
func setupRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	cache := filepath.Join(repo, "cache", "evolve")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	// 主仓库
	runCmd(t, repo, "git", "init", "-b", "main")
	gitc(t, repo, "commit", "--allow-empty", "-m", "base")
	// 子仓库（独立 git 仓库，被主仓库 gitignore）
	sub := filepath.Join(repo, "apps", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, sub, "git", "init", "-b", "main")
	gitc(t, sub, "commit", "--allow-empty", "-m", "sub base")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("apps/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitc(t, repo, "add", ".gitignore")
	gitc(t, repo, "commit", "-m", "ignore apps")
	return repo, cache
}

// addWorktree 模拟 evolve-driver 建树：主仓库 + 子仓库嵌套 worktree，分支 evolve/<taskNo>。
func addWorktree(t *testing.T, repo, cache, taskNo string) {
	t.Helper()
	dir := filepath.Join(cache, taskNo)
	runCmd(t, repo, "git", "worktree", "add", dir, "-b", "evolve/"+taskNo, "main")
	// 与 evolve-driver 一致：建树后 lock（防 prune）——清理路径必须能处理 locked worktree
	runCmd(t, repo, "git", "worktree", "lock", "--reason", "evolve-driver managed (do not prune)", dir)
	// 主仓库 worktree 里放一个提交
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(taskNo), 0o644); err != nil {
		t.Fatal(err)
	}
	gitc(t, dir, "add", "-A")
	gitc(t, dir, "commit", "-m", "change")
	// 子仓库嵌套 worktree
	subMain := filepath.Join(repo, "apps", "sub")
	subWT := filepath.Join(dir, "apps", "sub")
	runCmd(t, subMain, "git", "worktree", "add", subWT, "-b", "evolve/"+taskNo, "main")
	if err := os.WriteFile(filepath.Join(subWT, "s.txt"), []byte(taskNo), 0o644); err != nil {
		t.Fatal(err)
	}
	gitc(t, subWT, "add", "-A")
	gitc(t, subWT, "commit", "-m", "sub change")
}

// mergeToMain 把 evolve/<taskNo> 快进进主仓库与子仓库的 main。
func mergeToMain(t *testing.T, repo, taskNo string) {
	t.Helper()
	runCmd(t, repo, "git", "merge", "--ff-only", "evolve/"+taskNo)
	runCmd(t, filepath.Join(repo, "apps", "sub"), "git", "merge", "--ff-only", "evolve/"+taskNo)
}

// TestRemoveMerged 合并后 Remove：主 + 子 worktree 注册与目录都被删除；幂等（再删不报错）。
func TestRemoveMerged(t *testing.T) {
	repo, cache := setupRepo(t)
	m := newWorktreeManager(repo, cache, "main")
	addWorktree(t, repo, cache, "WI-1")
	mergeToMain(t, repo, "WI-1")

	if !m.BranchMerged("evolve/WI-1", "main") {
		t.Fatal("合并后 BranchMerged 应为 true")
	}
	if err := m.Remove("WI-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(m.Dir("WI-1")); !os.IsNotExist(err) {
		t.Fatal("目录应被删除")
	}
	// git 注册应消失
	wtList := exec.Command("git", "worktree", "list", "--porcelain")
	wtList.Dir = repo
	out, _ := wtList.Output()
	if strings.Contains(string(out), "WI-1") {
		t.Fatalf("git worktree 注册未清除: %s", out)
	}
	// 幂等
	if err := m.Remove("WI-1"); err != nil {
		t.Fatalf("Remove 幂等: %v", err)
	}
}

// TestScanSkipsUnmergedAndActive 未合并 / 进行中（active）的 worktree 绝不被扫出。
func TestScanSkipsUnmergedAndActive(t *testing.T) {
	repo, cache := setupRepo(t)
	m := newWorktreeManager(repo, cache, "main")
	addWorktree(t, repo, cache, "WI-merged")
	mergeToMain(t, repo, "WI-merged")
	addWorktree(t, repo, cache, "WI-unmerged") // 未合并
	addWorktree(t, repo, cache, "WI-active")   // 未合并但假设进行中

	// 全部 active → 一个都不清
	stale, err := m.Scan(map[string]bool{"WI-merged": true, "WI-unmerged": true, "WI-active": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("active 集合内不应有可清理项: %v", stale)
	}
	// 无 active → 只有已合并的 WI-merged
	stale, err = m.Scan(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "WI-merged" {
		t.Fatalf("应只扫出已合并的 WI-merged: %v", stale)
	}
}

// TestScanSkipsUncommitted 分支已合并进 base 但工作区有未提交改动（cancelled/打回任务：
// 分支天然是 base 祖先，目录里还有未提交 diff，rerun 续接依赖该目录）→ 绝不被扫出。
func TestScanSkipsUncommitted(t *testing.T) {
	repo, cache := setupRepo(t)
	m := newWorktreeManager(repo, cache, "main")
	addWorktree(t, repo, cache, "WI-dirty")
	mergeToMain(t, repo, "WI-dirty")
	// 合并后又在 worktree 里改了未提交文件（模拟打回后 agent 的未提交产出）
	if err := os.WriteFile(filepath.Join(cache, "WI-dirty", "f.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.HasUncommitted("WI-dirty") {
		t.Fatal("有未提交改动时 HasUncommitted 应为 true")
	}
	stale, err := m.Scan(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("有未提交改动的 worktree 不应被扫出: %v", stale)
	}
	// 提交后（工作区干净）→ 可清理
	gitc(t, filepath.Join(cache, "WI-dirty"), "add", "-A")
	gitc(t, filepath.Join(cache, "WI-dirty"), "commit", "-m", "commit dirty")
	if m.HasUncommitted("WI-dirty") {
		t.Fatal("提交后 HasUncommitted 应为 false")
	}
	// 注：提交后分支不再包含于 base → 仍不可清（未合并）；这里只验证脏判定翻转
	if m.BranchMerged("evolve/WI-dirty", "main") {
		t.Fatal("新提交后分支应不再包含于 main")
	}
}

// TestScanIgnoresNonWorktreeDirs 非 worktree 目录（无 .git 指针）不碰。
func TestScanIgnoresNonWorktreeDirs(t *testing.T) {
	repo, cache := setupRepo(t)
	m := newWorktreeManager(repo, cache, "main")
	if err := os.MkdirAll(filepath.Join(cache, "not-a-worktree"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale, err := m.Scan(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("非 worktree 目录不应被扫出: %v", stale)
	}
}

// TestBranchMerged 分支不存在 / base 不存在 → false（保守不清理）。
func TestBranchMerged(t *testing.T) {
	repo, _ := setupRepo(t)
	m := newWorktreeManager(repo, cacheDir(repo), "main")
	if m.BranchMerged("evolve/nope", "main") {
		t.Fatal("分支不存在应为 false")
	}
	if m.BranchMerged("main", "no-such-base") {
		t.Fatal("base 不存在应为 false")
	}
	if !m.BranchMerged("main", "main") {
		t.Fatal("main 已合并进 main 应为 true")
	}
}

func cacheDir(repo string) string { return filepath.Join(repo, "cache", "evolve") }
