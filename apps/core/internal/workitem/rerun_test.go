package workitem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResumeWorktreeState 续接时工作目录可用性判定（2026-10-29）。
//
// 旧实现只认目录：driver 的周期清理（无未提交改动就删目录）一跑，
// 「续接执行」就永久不可用 —— 但分支还在，driver 的 ensureWorktree 能重建。
func TestResumeWorktreeState(t *testing.T) {
	t.Run("目录在 → 直接复用", func(t *testing.T) {
		repo := t.TempDir()
		evolveDir := filepath.Join(repo, "cache", "evolve")
		taskNo := "WI-1-a"
		if err := os.MkdirAll(filepath.Join(evolveDir, taskNo), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := resumeWorktreeState(evolveDir, repo, taskNo)
		if err != nil || got != worktreeReuse {
			t.Fatalf("期望 reuse/nil，得到 %q/%v", got, err)
		}
	})

	t.Run("目录不在但分支在 → 重建", func(t *testing.T) {
		repo := t.TempDir()
		evolveDir := filepath.Join(repo, "cache", "evolve")
		taskNo := "WI-2-b"
		writeLooseRef(t, repo, "evolve/"+taskNo)
		got, err := resumeWorktreeState(evolveDir, repo, taskNo)
		if err != nil || got != worktreeRebuild {
			t.Fatalf("期望 rebuild/nil，得到 %q/%v", got, err)
		}
	})

	t.Run("目录不在、分支在 packed-refs → 重建", func(t *testing.T) {
		repo := t.TempDir()
		evolveDir := filepath.Join(repo, "cache", "evolve")
		taskNo := "WI-3-c"
		gitDir := filepath.Join(repo, ".git")
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		packed := "# pack-refs with: peeled fully-peeled sorted\n" +
			"1111111111111111111111111111111111111111 refs/heads/evolve/" + taskNo + "\n"
		if err := os.WriteFile(filepath.Join(gitDir, "packed-refs"), []byte(packed), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resumeWorktreeState(evolveDir, repo, taskNo)
		if err != nil || got != worktreeRebuild {
			t.Fatalf("期望 rebuild/nil，得到 %q/%v", got, err)
		}
	})

	t.Run("目录和分支都没了 → 报错并给出真实路径", func(t *testing.T) {
		repo := t.TempDir()
		evolveDir := filepath.Join(repo, "cache", "evolve")
		taskNo := "WI-4-d"
		got, err := resumeWorktreeState(evolveDir, repo, taskNo)
		if err == nil {
			t.Fatalf("两者都不在应报错，得到 %q", got)
		}
		// 报错必须带实际检查的绝对路径（旧实现打印的是硬编码 "cache/evolve/<no>"）
		if want := filepath.Join(evolveDir, taskNo); !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含实际路径 %q，得到 %q", want, err.Error())
		}
		if !strings.Contains(err.Error(), "重新执行") {
			t.Errorf("错误信息应指引「重新执行」，得到 %q", err.Error())
		}
	})

	t.Run("evolveDir 为空 → 不校验（复用）", func(t *testing.T) {
		got, err := resumeWorktreeState("", t.TempDir(), "WI-5-e")
		if err != nil || got != worktreeReuse {
			t.Fatalf("期望 reuse/nil，得到 %q/%v", got, err)
		}
	})
}

// TestBranchExists 分支判定：loose ref / packed-refs / 不存在 / 空参数。
func TestBranchExists(t *testing.T) {
	repo := t.TempDir()
	writeLooseRef(t, repo, "evolve/WI-x")

	if !branchExists(repo, "evolve/WI-x") {
		t.Error("loose ref 应存在")
	}
	if branchExists(repo, "evolve/WI-missing") {
		t.Error("不存在的分支不应返回 true")
	}
	if branchExists(repo, "") || branchExists("", "evolve/WI-x") {
		t.Error("空参数应返回 false")
	}
}

// writeLooseRef 造一个 loose ref 文件（.git/refs/heads/<branch>）。
func writeLooseRef(t *testing.T, repo, branch string) {
	t.Helper()
	p := filepath.Join(repo, ".git", "refs", "heads", branch)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("0000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
