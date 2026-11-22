package agentcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit 在 dir 执行 git（测试辅助；失败即 Fatal）。
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
	}
}

// initRepo 初始化一个临时 git 仓库并返回路径（含一次基线提交）。
func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	writeFile(t, filepath.Join(dir, "base.txt"), "base\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "base")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用，跳过")
	}
}

// TestCollectChanges 主仓库：已跟踪改动 + 未跟踪新文件都要进 diff 与清单，
// 且路径前缀为 b/（driver 的 scope 校验与行数统计按 +++ b/ 解析）。
func TestCollectChanges(t *testing.T) {
	requireGit(t)
	workdir := filepath.Join(t.TempDir(), "wt")
	initRepo(t, workdir)

	writeFile(t, filepath.Join(workdir, "base.txt"), "base\nmodified\n")
	writeFile(t, filepath.Join(workdir, "new.go"), "package main\n")

	diff, files := CollectChanges(workdir)
	for _, want := range []string{"+++ b/base.txt", "+++ b/new.go"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff 缺少 %q:\n%s", want, diff)
		}
	}
	if got := strings.Join(files, ","); got != "base.txt,new.go" {
		t.Errorf("files_changed=%q，期望 base.txt,new.go（排序去重）", got)
	}
}

// TestCollectChangesSubRepo 子仓库（独立 git，主仓库 gitignored）：diff 带 apps/<name>/ 前缀，
// 否则 driver 会按 apps/* 判定「越界」打回（历史事故）。
func TestCollectChangesSubRepo(t *testing.T) {
	requireGit(t)
	workdir := filepath.Join(t.TempDir(), "wt")
	initRepo(t, workdir)

	sub := filepath.Join(workdir, "apps", "dapp")
	initRepo(t, sub)
	writeFile(t, filepath.Join(sub, "src", "App.tsx"), "export const A = 1\n")
	runGit(t, sub, "add", ".")
	runGit(t, sub, "commit", "-m", "add app")
	writeFile(t, filepath.Join(sub, "src", "App.tsx"), "export const A = 2\n")
	writeFile(t, filepath.Join(sub, "src", "Messages.tsx"), "export const M = 1\n")

	// 主仓库同时有一处改动
	writeFile(t, filepath.Join(workdir, "base.txt"), "base\nmain-change\n")

	diff, files := CollectChanges(workdir)
	for _, want := range []string{
		"+++ b/base.txt",
		"+++ b/apps/dapp/src/App.tsx",
		"+++ b/apps/dapp/src/Messages.tsx",
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff 缺少 %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "//src") || strings.Contains(diff, "apps/dapp//") {
		t.Errorf("diff 出现双斜杠路径（历史越界误判根因）:\n%s", diff)
	}
	joined := strings.Join(files, ",")
	for _, want := range []string{"base.txt", "apps/dapp/src/App.tsx", "apps/dapp/src/Messages.tsx"} {
		if !strings.Contains(joined, want) {
			t.Errorf("files_changed 缺少 %q: %q", want, joined)
		}
	}
}

// TestSubRepoDirs 只认 apps/<name>/.git 存在的目录，且不带尾斜杠。
func TestSubRepoDirs(t *testing.T) {
	requireGit(t)
	workdir := filepath.Join(t.TempDir(), "wt")
	initRepo(t, workdir)
	initRepo(t, filepath.Join(workdir, "apps", "dapp"))
	if err := os.MkdirAll(filepath.Join(workdir, "apps", "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := SubRepoDirs(workdir)
	if len(got) != 1 || got[0] != "apps/dapp" {
		t.Fatalf("SubRepoDirs=%v，期望 [apps/dapp]（无尾斜杠）", got)
	}
}

// TestGitAvailable worktree 与非法目录的判定。
func TestGitAvailable(t *testing.T) {
	requireGit(t)
	workdir := filepath.Join(t.TempDir(), "wt")
	initRepo(t, workdir)
	if !GitAvailable(workdir) {
		t.Errorf("git 仓库应判定为可用")
	}
	if GitAvailable(t.TempDir()) {
		t.Errorf("非 git 目录应判定为不可用")
	}
}
