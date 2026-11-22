package agentcli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// gitRunRaw 执行 git 命令，返回 stdout 与退出码。
// 退出码非 0 不算 error（`git diff --no-index` 有差异时退出 1，属正常），只有进程起不来才返回 err。
func gitRunRaw(dir string, args ...string) (string, int, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.String(), ee.ExitCode(), nil
	}
	return "", -1, err
}

// gitOut 执行 git 并返回 stdout（失败返回空串）。
func gitOut(dir string, args ...string) string {
	out, _, err := gitRunRaw(dir, args...)
	if err != nil {
		return ""
	}
	return out
}

// gitArgs 默认在 repoDir 下执行（-C 与 dir 二选一，统一用 -C 便于传相对路径子仓库）。
func gitIn(repoDir string, args ...string) []string {
	return append([]string{"-C", repoDir}, args...)
}

// GitAvailable worktree 是否为可用 git 工作树。
func GitAvailable(workdir string) bool {
	return strings.TrimSpace(gitOut("", gitIn(workdir, "rev-parse", "--is-inside-work-tree")...)) == "true"
}

// EnsureSafeDirectory 容器用户 ≠ worktree 属主（宿主用户，如 1001）→ git 报
// "dubious ownership" 而拒绝操作。只对当前 worktree 精确加白（幂等，避免污染其它目录）。
func EnsureSafeDirectory(dir string) {
	if dir == "" {
		return
	}
	for _, line := range strings.Split(gitOut("", "config", "--global", "--get-all", "safe.directory"), "\n") {
		if strings.TrimSpace(line) == dir {
			return
		}
	}
	_, _, _ = gitRunRaw("", "config", "--global", "--add", "safe.directory", dir)
}

// SubRepoDirs 子仓库（嵌套 worktree）：apps/<name>/.git 存在 = 独立 git 仓库
// （主仓库 .gitignore 忽略其内容，故主仓库 git 看不到它们）。
// 返回相对 workdir 的路径且**不带尾斜杠**（拼路径会产生 apps/dapp//src 双斜杠，
// 曾导致 scope 校验误判越界）。
func SubRepoDirs(workdir string) []string {
	matches, err := filepath.Glob(filepath.Join(workdir, "apps", "*"))
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range matches {
		if _, err := os.Stat(filepath.Join(m, ".git")); err != nil {
			continue
		}
		rel, err := filepath.Rel(workdir, m)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

// UntrackedFiles 仓库内未跟踪文件（相对该仓库根）。
func UntrackedFiles(repoDir string) []string {
	out := gitOut("", gitIn(repoDir, "ls-files", "--others", "--exclude-standard", "-z")...)
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// HasChanges worktree（主仓库 + 子仓库）是否有任何未提交改动（含未跟踪文件）。
// 比 CollectChanges 便宜：只看 status，不生成 diff。用于「只读不写」检测——
// agent 大量工具调用却零写入时据此判定探索死循环。
func HasChanges(workdir string) bool {
	if workdir == "" {
		return false
	}
	if strings.TrimSpace(gitOut("", gitIn(workdir, "status", "--porcelain")...)) != "" {
		return true
	}
	for _, sr := range SubRepoDirs(workdir) {
		if strings.TrimSpace(gitOut("", gitIn(filepath.Join(workdir, sr), "status", "--porcelain")...)) != "" {
			return true
		}
	}
	return false
}

// CollectChanges 收集 worktree（主仓库 + 子仓库）相对 HEAD 的**累计**改动。
//
// 不用「运行前后快照对比」：任务重启复用 worktree（session 续接）时，上一轮的未提交改动
// 必须被上报（否则 driver 看到 empty diff 打回，而那正是要评审/合并的内容）；
// 全新运行时 worktree 已被 driver 的 resetWorktree 清干净，不会误报陈旧改动。
//
// 子仓库 diff 经 --src-prefix/--dst-prefix 加 apps/<name>/ 前缀：scope 校验按 apps/*
// 判定、reviewer 评审、行数统计三者同口径（此前缺前缀被误判「越界」打回）。
// 返回 diff（多块用 "\n" 连接）与排序去重后的变更文件清单。
func CollectChanges(workdir string) (string, []string) {
	var parts []string
	changed := map[string]bool{}

	collect := func(repoDir, prefix string) {
		diffArgs := []string{"diff", "HEAD"}
		if prefix != "" {
			// 注：git 会自行在 prefix 后补一个 "/"，故这里**不能**带尾斜杠
			// （带了会产出 apps/dapp//src 双斜杠，历史越界误判根因）。
			diffArgs = append(diffArgs, "--src-prefix=a/"+prefix, "--dst-prefix=b/"+prefix)
		}
		if d := strings.TrimRight(gitOut("", gitIn(repoDir, diffArgs...)...), "\n"); d != "" {
			parts = append(parts, d)
		}
		for _, f := range UntrackedFiles(repoDir) {
			args := []string{"diff", "--no-index"}
			if prefix != "" {
				args = append(args, "--src-prefix=a/"+prefix, "--dst-prefix=b/"+prefix)
			}
			args = append(args, "--", "/dev/null", f)
			if d := strings.TrimRight(gitOut("", gitIn(repoDir, args...)...), "\n"); d != "" {
				parts = append(parts, d)
			}
		}
		for _, line := range strings.Split(gitOut("", gitIn(repoDir, "diff", "HEAD", "--name-only")...), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				changed[prefix+line] = true
			}
		}
		for _, line := range strings.Split(gitOut("", gitIn(repoDir, "ls-files", "--others", "--exclude-standard")...), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				changed[prefix+line] = true
			}
		}
	}

	collect(workdir, "")
	for _, sr := range SubRepoDirs(workdir) {
		collect(filepath.Join(workdir, sr), sr+"/")
	}

	files := make([]string, 0, len(changed))
	for f := range changed {
		files = append(files, f)
	}
	sort.Strings(files)
	return strings.Join(parts, "\n"), files
}
