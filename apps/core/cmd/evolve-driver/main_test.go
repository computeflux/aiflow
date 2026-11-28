package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGithubOwnerRepo 锁定 PR 链接所需的 owner/repo 解析。
func TestGithubOwnerRepo(t *testing.T) {
	owner, repo, err := githubOwnerRepo("https://github.com/computeflux/workflow.git")
	if err != nil || owner != "computeflux" || repo != "workflow" {
		t.Fatalf("got %s/%s err=%v", owner, repo, err)
	}
	owner, repo, _ = githubOwnerRepo("https://github.com/computeflux/workflow")
	if owner != "computeflux" || repo != "workflow" {
		t.Fatalf("无 .git 后缀解析错: %s/%s", owner, repo)
	}
	if _, _, err := githubOwnerRepo("nonsense"); err == nil {
		t.Fatal("非法远端应报错")
	}
}

// TestRedactSecretAndFirstLine 锁定 token 脱敏与 PR 标题取首行。
func TestRedactSecretAndFirstLine(t *testing.T) {
	if got := redactSecret("fatal: auth ghp_abc123 rejected", "ghp_abc123"); strings.Contains(got, "ghp_abc123") {
		t.Fatalf("token 未脱敏: %s", got)
	}
	if got := firstLine("第一行\n第二行", 120); got != "第一行" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("", 10); got != "" {
		t.Fatalf("空串 = %q", got)
	}
}

// TestCIGateError 锁定 CI 门禁（§3.8 全绿才进审批）：contract 非 ok / failed>0 / 字段缺失 → 拦截。
func TestCIGateError(t *testing.T) {
	cases := []struct {
		out  map[string]any
		want bool // true = 应拦截
		name string
	}{
		{map[string]any{"contract": "ok", "failed": float64(0)}, false, "全绿放行"},
		{map[string]any{"contract": "fail", "failed": float64(0)}, true, "contract 失败拦截"},
		{map[string]any{"contract": "ok", "failed": float64(2)}, true, "有失败用例拦截"},
		{map[string]any{}, true, "字段缺失 fail-closed"},
	}
	for _, c := range cases {
		if err := ciGateError(c.out); (err != nil) != c.want {
			t.Errorf("ciGateError(%v) = %v, want 拦截=%v（%s）", c.out, err, c.want, c.name)
		}
	}
}

// TestIsPytestNoTests 锁定“pytest exit 5 = 未收集到用例”容忍口径（与 ci-runner.sh 一致）：
// 仅 pytest 命令的 exit 5 放行留痕；其它命令/其它退出码一律不算。
func TestIsPytestNoTests(t *testing.T) {
	pytestCmd := []string{"sh", "-c", "PYTHONHOME=/work/.venv/python .venv/python/bin/python -m pytest -q"}
	goCmd := []string{"go", "test", "./..."}
	cases := []struct {
		cmd  []string
		rc   int
		want bool
		name string
	}{
		{pytestCmd, 5, true, "pytest 未收集到用例"},
		{pytestCmd, 1, false, "pytest 真实失败"},
		{pytestCmd, 0, false, "pytest 通过"},
		{goCmd, 5, false, "go test 非零不算未收集"},
		{nil, 5, false, "空命令不算"},
	}
	for _, c := range cases {
		if got := isPytestNoTests(c.cmd, c.rc); got != c.want {
			t.Errorf("isPytestNoTests(%v, %d) = %v, want %v（%s）", c.cmd, c.rc, got, c.want, c.name)
		}
	}
}

// TestEffectiveTaskTarget 锁定「卡上目标优先」的刷新口径（2026-11-27 卡 #10）：
// 卡上后补勾选的目标（如 core）必须覆盖进化单建单时的旧快照 social；
// 卡目标为空时保留任务原目标（不清空，避免中断在跑任务）。
func TestEffectiveTaskTarget(t *testing.T) {
	cases := []struct {
		name, task, card, want string
		changed                bool
	}{
		{"卡后补勾 core", "social", "social,core,core-ui", "social,core,core-ui", true},
		{"卡目标归一/别名", "social", " api , social ,core", "core,social", true},
		{"卡为空保留快照", "social", "", "social", false},
		{"归一后一致不更新", "social,core", " social , core ", "social,core", false},
	}
	for _, c := range cases {
		got, changed := effectiveTaskTarget(c.task, c.card)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: effectiveTaskTarget(%q, %q) = (%q, %v), want (%q, %v)",
				c.name, c.task, c.card, got, changed, c.want, c.changed)
		}
	}
}

// TestOutOfBoundsFiles 锁定越界判定：非 apps/* 或 apps/board* / apps/core* 都是越界
// （core = Go 核心 / core-ui = board 前端，人工维护，只走人工 PR）；
// 但任务显式选中的目标目录放行；路径归一化（双斜杠/./）不误报。
func TestOutOfBoundsFiles(t *testing.T) {
	got := outOfBoundsFiles([]string{
		"apps/pyreplay/like.py",             // ok
		"apps/board-ops/src/a.tsx",          // 越界（board* 人工维护）
		"apps/core-ui/src/ops/a.tsx",        // 越界（core* 前缀命中 core-ui，人工维护）
		"apps/core/internal/db/models.go",   // 越界（Go 核心，人工 PR）
		"apps/core/cmd/api/main.go",         // 越界（Go 核心，人工 PR）
		"deploy/docker-compose.yml",         // 越界（非 apps/*）
		"",                                  // 忽略
		"apps/c2c/main.go",                  // ok
		"apps/dapp//src/pages/Messages.tsx", // ok（双斜杠归一化；子仓库前缀拼接历史 bug）
	}, nil)
	want := []string{"apps/board-ops/src/a.tsx", "apps/core-ui/src/ops/a.tsx", "apps/core/internal/db/models.go", "apps/core/cmd/api/main.go", "deploy/docker-compose.yml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("outOfBoundsFiles = %v, want %v", got, want)
	}

	// 显式选择 core / core-ui / dapp 为目标 → 对应目录放行（选择即授权）
	got = outOfBoundsFiles([]string{
		"apps/core-ui/src/product/pages/BoardPage.tsx",
		"apps/core/internal/workitem/dispatch.go",
		"apps/dapp/src/App.tsx",
		"apps/board-ops/src/a.tsx", // 未选中 → 仍越界
	}, []string{"actives", "tee-node", "dapp", "core-ui", "core"})
	want = []string{"apps/board-ops/src/a.tsx"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("outOfBoundsFiles(explicit targets) = %v, want %v", got, want)
	}

	// 未选中 core* → 仍越界
	got = outOfBoundsFiles([]string{"apps/core/internal/db/models.go"}, []string{"actives"})
	if len(got) != 1 {
		t.Errorf("outOfBoundsFiles(no core target) = %v, want 1 越界", got)
	}
}

// TestCollectChangedFiles 锁定改动文件收集：files_changed + diff 头兜底，去重。
func TestCollectChangedFiles(t *testing.T) {
	out := &coderOut{
		FilesChanged: []string{"apps/pyreplay/a.py", "internal/db/models.go"},
		Diff: "diff --git a/internal/db/models.go b/internal/db/models.go\n" +
			"--- a/internal/db/models.go\n+++ b/internal/db/models.go\n@@ -1 +1 @@\n-x\n+y\n" +
			"diff --git a/apps/pyreplay/b.py b/apps/pyreplay/b.py\n" +
			"+++ b/apps/pyreplay/b.py\n@@ -1 +1 @@\n-a\n+b\n",
	}
	got := collectChangedFiles(out)
	want := []string{"apps/pyreplay/a.py", "internal/db/models.go", "apps/pyreplay/b.py"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("collectChangedFiles = %v, want %v", got, want)
	}
}

// TestCountDiffChangedLines 锁定越界文件改动行数统计（+/- 行，不含文件头；按文件过滤）。
func TestCountDiffChangedLines(t *testing.T) {
	diff := "diff --git a/apps/pyreplay/a.py b/apps/pyreplay/a.py\n" +
		"--- a/apps/pyreplay/a.py\n+++ b/apps/pyreplay/a.py\n" +
		"@@ -1,2 +1,3 @@\n keep\n-old\n+new\n+added\n" +
		"diff --git a/internal/db/models.go b/internal/db/models.go\n" +
		"--- a/internal/db/models.go\n+++ b/internal/db/models.go\n" +
		"@@ -10,1 +10,2 @@\n type X struct {\n-    A int\n+    A int\n+    RetryCount int\n }\n"
	// 只有 internal/db/models.go 是越界文件：该文件 3 行改动（-1 +2），不含其它文件的 keep/新增
	if got := countDiffChangedLines(diff, []string{"internal/db/models.go"}); got != 3 {
		t.Errorf("越界行数 = %d, want 3", got)
	}
	// 多文件：apps/pyreplay/a.py 3 行（-1 +2）
	if got := countDiffChangedLines(diff, []string{"apps/pyreplay/a.py"}); got != 3 {
		t.Errorf("正常文件行数 = %d, want 3", got)
	}
	// 空 diff → 0（调用方对 0 行越界按打回处理）
	if got := countDiffChangedLines("", []string{"internal/db/models.go"}); got != 0 {
		t.Errorf("空 diff 行数 = %d, want 0", got)
	}
}

// TestParseWorktreeForBranch 锁定「base 被哪个工作区检出」的解析（落地前守卫依赖它）：
// 误判会让 ref 前进而工作区停在旧提交，之后提交就把 AI 改动静默回退（实测丢过 4 次）。
func TestParseWorktreeForBranch(t *testing.T) {
	porcelain := "worktree /repo\n" +
		"HEAD 3d22b18aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"branch refs/heads/main\n" +
		"\n" +
		"worktree /repo/cache/evolve/WI-1\n" +
		"HEAD 2bbfa10aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"branch refs/heads/evolve/WI-1\n" +
		"locked evolve-driver managed (do not prune)\n" +
		"\n" +
		"worktree /repo/detached\n" +
		"HEAD 891134caaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"detached\n" +
		"\n"

	if got := parseWorktreeForBranch(porcelain, "main"); got != "/repo" {
		t.Errorf("main 检出路径 = %q, want /repo", got)
	}
	if got := parseWorktreeForBranch(porcelain, "evolve/WI-1"); got != "/repo/cache/evolve/WI-1" {
		t.Errorf("evolve 分支检出路径 = %q", got)
	}
	// detached 块不能被误认；未检出的分支返回空（→ 不触发守卫）
	if got := parseWorktreeForBranch(porcelain, "release"); got != "" {
		t.Errorf("未检出分支 = %q, want 空", got)
	}
	if got := parseWorktreeForBranch("", "main"); got != "" {
		t.Errorf("空输出 = %q, want 空", got)
	}
	// detached 块在前、目标块在后：不能在 detached 块上误报路径
	detachedFirst := "worktree /repo/detached\nHEAD abc\ndetached\n\n" +
		"worktree /repo\nHEAD def\nbranch refs/heads/main\n\n"
	if got := parseWorktreeForBranch(detachedFirst, "main"); got != "/repo" {
		t.Errorf("detached 在前时 = %q, want /repo", got)
	}
}

// TestMergePaused 锁定「main 有未提交改动 → 暂停合入，每 MergeRetrySec 重试一次」的闸门判定：
// 误判会让脏工作区被推进（→ 后续提交静默回退 AI 改动），或让任务永久卡住不合入。
func TestMergePaused(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if mergePaused(0, now) {
		t.Error("until=0（未暂停）应放行")
	}
	if !mergePaused(now.Add(5*time.Minute).UnixNano(), now) {
		t.Error("重试时刻未到 → 应保持暂停（不要每分钟重试）")
	}
	if mergePaused(now.Add(-time.Second).UnixNano(), now) {
		t.Error("已过重试时刻 → 应放行检查")
	}
	if mergePaused(now.UnixNano(), now) {
		t.Error("恰好到点应放行检查（5 分钟到点即查）")
	}
}

// TestDirtyRepoError 锁定「相关仓库工作区不干净 → 暂停合入」的错误组装：
// 必须带仓库名与路径（看板/audit 要能定位到是主仓库还是哪个子仓库），
// 且必须匹配 errBaseWorktreeDirty（否则会被当成永久失败写 pr_error，不再自动重试）。
func TestDirtyRepoError(t *testing.T) {
	mainErr := dirtyRepoError("", "/repo")
	if !errors.Is(mainErr, errBaseWorktreeDirty) {
		t.Error("主仓库脏必须匹配 errBaseWorktreeDirty")
	}
	if !strings.Contains(mainErr.Error(), "主仓库") || !strings.Contains(mainErr.Error(), "/repo") {
		t.Errorf("主仓库错误缺少定位信息: %v", mainErr)
	}
	subErr := dirtyRepoError("contract-deploy", "/repo/apps/contract-deploy")
	if !errors.Is(subErr, errBaseWorktreeDirty) {
		t.Error("子仓库脏同样要匹配 errBaseWorktreeDirty（走重试而非永久失败）")
	}
	if !strings.Contains(subErr.Error(), "contract-deploy") || !strings.Contains(subErr.Error(), "/repo/apps/contract-deploy") {
		t.Errorf("子仓库错误缺少定位信息: %v", subErr)
	}
}

// TestReviewerRetryBackoffs 锁定 reviewer 执行失败的重试退避序列：
// reviewer 不可用（provider 400/429/超时）必须重试而不是直接把任务判死（2026-10-25 实测被 cancel）。
func TestReviewerRetryBackoffs(t *testing.T) {
	got := reviewerRetryBackoffs(3, 15*time.Second)
	want := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("重试次数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次退避 = %s, want %s", i+1, got[i], want[i])
		}
	}
	// 不重试 / 非法入参：不得产生退避（避免误当成「无限重试」或 0 延迟空转）
	if got := reviewerRetryBackoffs(0, 15*time.Second); len(got) != 0 {
		t.Errorf("retries=0 应不重试，got=%v", got)
	}
	if got := reviewerRetryBackoffs(-1, 15*time.Second); len(got) != 0 {
		t.Errorf("retries<0 应不重试，got=%v", got)
	}
	// base<=0 回退到 15s（防配置为 0 时变成疯狂重试）
	if got := reviewerRetryBackoffs(1, 0); len(got) != 1 || got[0] != 15*time.Second {
		t.Errorf("base=0 应回退 15s，got=%v", got)
	}
}

// TestMergeGateActive 锁定「未合入 → 阻塞新任务」门禁只在本地合入模式生效：
// pr/off 模式不做本地合入（merge_commit 永远为空），若也上门禁会把整条管线永久卡死。
func TestMergeGateActive(t *testing.T) {
	for _, mode := range []string{"local", "both"} {
		if !mergeGateActive(mode) {
			t.Errorf("mode=%s 应启用阻塞门禁", mode)
		}
	}
	for _, mode := range []string{"pr", "off", ""} {
		if mergeGateActive(mode) {
			t.Errorf("mode=%s 不得启用阻塞门禁（否则管线被永久卡死）", mode)
		}
	}
}

// TestBranchHasChangesToLand 锁定「哪些仓库算本次任务相关」的判据：
// 只有分支上有本任务提交（base..branch > 0）的仓库才参与合并与工作区脏检查。
// 分支落后 base（本任务没改它）绝不能被算作相关——否则无关子仓库的未提交改动会卡住整单合入
// （2026-10-27 实测：只改 apps/dapp 的任务被 apps/tee-node 的 hack/.version 卡住）。
func TestBranchHasChangesToLand(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "base.txt", "base", "base")
	gitIn(t, dir, "branch", "evolve/WI-1")

	// 分支 == base
	if has, err := branchHasChangesToLand(dir, "evolve/WI-1", "main"); err != nil || has {
		t.Fatalf("分支=base: has=%v err=%v，want false/nil", has, err)
	}

	// base 前进、分支原地不动（落后）→ 无本任务提交（旧判据会误判为相关）
	commitFile(t, dir, "other.txt", "other", "base2")
	if has, err := branchHasChangesToLand(dir, "evolve/WI-1", "main"); err != nil || has {
		t.Fatalf("分支落后 base: has=%v err=%v，want false/nil", has, err)
	}

	// 分支有本任务提交（与 base 分叉）→ 有改动
	gitIn(t, dir, "checkout", "-q", "evolve/WI-1")
	commitFile(t, dir, "task.txt", "task", "task")
	if has, err := branchHasChangesToLand(dir, "evolve/WI-1", "main"); err != nil || !has {
		t.Fatalf("分支有本任务提交: has=%v err=%v，want true/nil", has, err)
	}

	// 分支提交已被 base 吸收 → 不再需要落地
	gitIn(t, dir, "checkout", "-q", "main")
	gitIn(t, dir, "merge", "-q", "--no-ff", "-m", "merge task", "evolve/WI-1")
	if has, err := branchHasChangesToLand(dir, "evolve/WI-1", "main"); err != nil || has {
		t.Fatalf("分支已合入 base: has=%v err=%v，want false/nil", has, err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", msg)
}

// TestPrefixAIFlowReviewPattern 锁定 .aiflow review_ignore 的路径归一：
// 相对 apps/<app>/ 的条目必须拼成仓库相对路径（与 diff / reviewer 评论同口径）。
func TestPrefixAIFlowReviewPattern(t *testing.T) {
	cases := []struct{ app, in, want string }{
		{"minishop", "src/contracts/evm/tokenSol.ts", "apps/minishop/src/contracts/evm/tokenSol.ts"},
		{"minishop", "  gen/**  ", "apps/minishop/gen/**"},
		{"minishop", "apps/c2c/index.html", "apps/c2c/index.html"},
		{"minishop", "/apps/x/a.ts", "apps/x/a.ts"},
		{"minishop", "", ""},
	}
	for _, c := range cases {
		if got := prefixAIFlowReviewPattern(c.app, c.in); got != c.want {
			t.Errorf("prefixAIFlowReviewPattern(%q, %q) = %q, want %q", c.app, c.in, got, c.want)
		}
	}
}
