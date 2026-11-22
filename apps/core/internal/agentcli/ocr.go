package agentcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OCREnv reviewer adapter 运行环境（driver 经 env 注入 + 容器 env 兜底）。
type OCREnv struct {
	Bin         string // OCR_BIN（默认 $HOME/.npm-global/bin/ocr，再兜底 PATH）
	Provider    string // OCR_PROVIDER（必填：确定性运行，缺省快速失败而非交互式提示）
	Model       string // OCR_MODEL（可选）
	Concurrency int    // OCR_CONCURRENCY（默认 2：降低 LLM 并发压力）
	Effort      string // OCR_EFFORT（low|medium|high，空 = ocr 默认 medium）
	TimeoutSec  int    // OCR_TIMEOUT_SEC（默认 3600：并发任务总预算，含 LLM 请求与重试）
	LLMTimeout  int    // OCR_LLM_TIMEOUT（默认 1800：单请求 LLM 超时，秒）
}

// LoadOCREnv 从环境变量组装配置。
func LoadOCREnv() OCREnv {
	bin := strings.TrimSpace(os.Getenv("OCR_BIN"))
	if bin == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".npm-global", "bin", "ocr")
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				bin = p
			}
		}
	}
	if bin == "" {
		if p, err := exec.LookPath("ocr"); err == nil {
			bin = p
		}
	}
	return OCREnv{
		Bin:         bin,
		Provider:    strings.TrimSpace(os.Getenv("OCR_PROVIDER")),
		Model:       strings.TrimSpace(os.Getenv("OCR_MODEL")),
		Concurrency: envInt("OCR_CONCURRENCY", 2),
		Effort:      strings.TrimSpace(os.Getenv("OCR_EFFORT")),
		TimeoutSec:  envInt("OCR_TIMEOUT_SEC", 3600),
		LLMTimeout:  envInt("OCR_LLM_TIMEOUT", 1800),
	}
}

// RunOCR reviewer adapter（op=review）：跑 ocr CLI 评审 workdir 的 git 变更，
// 把输出映射为协议评论（error|warning|info）。
func RunOCR(in io.Reader, stdout, stderr io.Writer) error {
	p := &protoIO{stdout: stdout, stderr: stderr}

	var input ReviewInput
	if err := p.readInput(in, &input); err != nil {
		return err
	}
	if input.Op != "review" {
		return p.fail("unsupported op: %s", input.Op)
	}
	if input.Workdir == "" {
		return p.fail("missing workdir")
	}
	if st, err := os.Stat(input.Workdir); err != nil || !st.IsDir() {
		return p.fail("workdir not found: %s", input.Workdir)
	}

	env := LoadOCREnv()
	if env.Bin == "" {
		return p.fail("ocr CLI not found (npm i -g @alibaba-group/open-code-review)")
	}
	if env.Provider == "" {
		// 必须显式提供 provider：ocr 无配置时会在 /dev/tty 交互式提示而挂起
		return p.fail("reviewer provider not configured: set OCR_PROVIDER env or run 'ocr config provider'")
	}
	EnsureSafeDirectory(input.Workdir)

	timeout := env.TimeoutSec
	if timeout <= 0 {
		timeout = 3600
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	// 主仓库 + 每个子仓库（嵌套 worktree）分别评审：子仓库是独立 git 仓库，主仓库
	// git 看不到其内部改动（gitlink），必须进子仓库 worktree 单独跑 ocr，否则
	// 「no items were selected」→ 0 条评论（2026-10-27 实测：tee-node 子仓库任务）。
	// 子仓库评论的 file 路径加 apps/<name>/ 前缀，与 diff / scope 校验同口径。
	type repoTarget struct {
		dir    string
		prefix string
	}
	targets := []repoTarget{{dir: input.Workdir, prefix: ""}}
	for _, sr := range SubRepoDirs(input.Workdir) {
		targets = append(targets, repoTarget{dir: filepath.Join(input.Workdir, sr), prefix: sr + "/"})
	}

	// 生成产物（锁文件 / 构建输出 / ABI 绑定 / .env 等）不送审：
	// 内置默认表 + apps/<app>/.aiflow 的 review_ignore（路径由 driver 经 OCR_EXCLUDE 传入）。
	// 它们没有可“修”的语义，否则 reviewer 会反复报 error 把任务卡在重修循环（2026-10-28）。
	exclude := ReviewExcludePatterns(SplitExcludePatterns(os.Getenv("OCR_EXCLUDE")))

	var all []Comment
	sessions := map[string]bool{}
	start := time.Now()
	if input.StartUnix > 0 {
		start = time.Unix(input.StartUnix, 0)
	}
	for _, t := range targets {
		EnsureSafeDirectory(t.dir)
		raw, err := runOCRRepo(ctx, env, t.dir, input.Base, timeout, exclude)
		if err != nil {
			return p.fail("%s", err.Error())
		}
		// 本轮 ocr 会话 ID（board 卡片「reviewer 会话」入口据此定位会话文件）：
		// 扫本仓库会话目录里 mtime 不早于本次启动的会话（多轮评审取最新一个）。
		if sid := latestRepoSession(t.dir, start); sid != "" {
			sessions[sid] = true
		}
		comments := parseOCRComments(raw)
		// 兜底过滤：即使 ocr 的 --exclude 未生效（版本差异/规则覆盖），生成产物评论也不进入打回判定
		if len(comments) > 0 {
			kept := make([]Comment, 0, len(comments))
			skipped := 0
			for _, c := range comments {
				if c.File != "" && ReviewExcludedWithPatterns(c.File, exclude) {
					skipped++
					continue
				}
				kept = append(kept, c)
			}
			if skipped > 0 {
				p.logf("review ignore: dropped %d comments on generated files", skipped)
			}
			comments = kept
		}
		if t.prefix != "" {
			for i := range comments {
				if comments[i].File != "" {
					comments[i].File = t.prefix + comments[i].File
				}
			}
		}
		p.logf("ocr repo=%s comments=%d", t.dir, len(comments))
		all = append(all, comments...)
	}
	return p.emit(ReviewOutput{OK: true, Comments: dedupComments(all), Sessions: sessionList(sessions)})
}

// latestRepoSession 返回该仓库（按 ocr 的 repo 目录哈希分目录）在 start 之后启动的最新会话 ID。
// 任何失败返回空串（尽力而为，绝不阻断评审）。
func latestRepoSession(repoDir string, start time.Time) string {
	root := filepath.Join(ocrSessionsRoot(), repoHashName(repoDir))
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	best := ""
	var bestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		st, err := e.Info()
		if err != nil || st.ModTime().Before(start.Add(-5*time.Second)) {
			continue // 历史会话（早于本次运行）不算
		}
		if best == "" || st.ModTime().After(bestTime) {
			best, bestTime = strings.TrimSuffix(e.Name(), ".jsonl"), st.ModTime()
		}
	}
	return best
}

// ocrSessionsRoot ocr 会话根目录（$OCR_SESSION_DIR，默认 ~/.opencodereview/sessions）。
func ocrSessionsRoot() string {
	if v := strings.TrimSpace(os.Getenv("OCR_SESSION_DIR")); v != "" {
		return filepath.Join(v, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".opencodereview", "sessions")
}

// repoHashName ocr 的仓库会话目录名：绝对路径 → '/' 换 '-'、去前导 '-'。
// （与 ocr CLI 目录命名同规则；两者必须同步，否则 board 侧拼不出会话文件路径。）
func repoHashName(repoDir string) string {
	n := strings.ReplaceAll(filepath.Clean(repoDir), "/", "-")
	return strings.TrimPrefix(n, "-")
}

// sessionList map → 排序切片。
func sessionList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// runOCRRepo 在单个 git 仓库（主仓库或子仓库 worktree）内跑 ocr，返回原始 JSON 输出。
// 超时 / 非零退出 / 评审 failed 均返回 error（由调用方 fail 上报）。
func runOCRRepo(ctx context.Context, env OCREnv, repoDir, base string, timeout int, exclude []string) (string, error) {
	args := []string{"review", "--format", "json", "--audience", "agent", "--provider", env.Provider}
	if env.Model != "" {
		args = append(args, "--model", env.Model)
	}
	args = append(args, "--concurrency", strconv.Itoa(env.Concurrency))
	// ocr 内置「并发任务总预算」--timeout 单位是分钟；与外侧进程超时对齐。
	minutes := timeout / 60
	if minutes < 1 {
		minutes = 1
	}
	args = append(args, "--timeout", strconv.Itoa(minutes))
	if env.Effort != "" {
		args = append(args, "--effort", env.Effort)
	}
	// 生成产物不送审（内置默认表 + apps/<app>/.aiflow 的 review_ignore）。
	if len(exclude) > 0 {
		args = append(args, "--exclude", strings.Join(exclude, ","))
	}
	// ocr 1.9.x：--from 必须配 --to。worktree 场景 coder 改动为未提交工作区变更
	//（HEAD == base）→ 不带 --from/--to，review 工作区（staged+unstaged+untracked）；
	// HEAD 领先 base（有 commit）→ 分支 diff 模式 --from base --to HEAD。
	if base != "" {
		if strings.TrimSpace(gitOut("", gitIn(repoDir, "rev-list", base+"..HEAD")...)) != "" {
			args = append(args, "--from", base, "--to", "HEAD")
		}
	}

	cmd := exec.CommandContext(ctx, env.Bin, args...)
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader("") // </dev/null：避免 ocr 交互式提示挂起
	// 单请求 LLM 超时：默认 300 偏小（reasoning 模型单轮思考常超）→ 放宽，外侧进程超时兜底。
	cmd.Env = append(os.Environ(), "OCR_LLM_TIMEOUT="+strconv.Itoa(env.LLMTimeout))
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	runErr := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("ocr timeout after %ds: stderr=%s", timeout, tail(strings.TrimSpace(errOut.String()), 300))
	}
	if runErr != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		}
		detail := strings.TrimSpace(out.String() + "\n" + errOut.String())
		return "", fmt.Errorf("ocr exited with code %d: %s", code, tail(detail, 500))
	}
	// JSON 结果在 stdout（--audience agent 时进度只在 stderr），解析不受进度行干扰。
	trimmedOut := strings.TrimSpace(out.String())
	// ocr v1.13 的 --format json 是「run manifest」（status/manifest/coverage + comments）：
	// 必须区分「评审完且无问题」与「评审未做完」（skipped/failed）——
	// 后者若当干净通过，会把 failed 评审放行到上游，与 error 级即打回的设计相背。
	if status, failed, isManifest := ocrRunManifest(trimmedOut); isManifest {
		if status == "failed" || failed > 0 {
			return "", fmt.Errorf("ocr review failed (status=%s, failed_items=%d): %s",
				status, failed, tail(trimmedOut, 400))
		}
	}
	return trimmedOut, nil
}

// ---- ocr 输出 → 协议评论 ----

// mustJSON 序列化（失败返回空串；仅用于兜底展示）。
func mustJSON(v any) string {
	s, err := jsonCompact(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return s
}
