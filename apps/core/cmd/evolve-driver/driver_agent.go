package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wetee/ai-workflow/internal/agentcli"
	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/db"
)

// refreshModelsFromProfile（S8 模型管理）：model_profiles 表 active 档案的 consumers
// 含 coder/reviewer 时，覆盖 cfg.CoderModel / ReviewerModel（下次进化执行生效）。
// 无 active / 查询失败 = 保持 env 配置不动。仅在 tick 拾取新任务前调用。
func (d *driver) refreshModelsFromProfile() {
	p, err := d.store.ActiveModelConfig()
	if err != nil || p == nil {
		return // 无 active 档案：保持 env
	}
	var consumers []string
	for _, c := range strings.Split(p.Consumers, ",") {
		if c = strings.TrimSpace(c); c != "" {
			consumers = append(consumers, c)
		}
	}
	apply := func(consumer, envModel string) string {
		for _, c := range consumers {
			if c == consumer && p.Model != "" && p.Model != envModel {
				log.Printf("model-config: %s model %q → %q（active 档案 %q）", consumer, envModel, p.Model, p.Name)
				return p.Model
			}
		}
		return envModel
	}
	if len(consumers) == 0 { // consumers 空 = 应用到全部消费面
		if p.Model != "" && p.Model != d.cfg.CoderModel {
			log.Printf("model-config: coder model %q → %q（active 档案 %q）", d.cfg.CoderModel, p.Model, p.Name)
			d.cfg.CoderModel = p.Model
		}
		if p.Model != "" && p.Model != d.cfg.ReviewerModel {
			log.Printf("model-config: reviewer model %q → %q（active 档案 %q）", d.cfg.ReviewerModel, p.Model, p.Name)
			d.cfg.ReviewerModel = p.Model
		}
		return
	}
	d.cfg.CoderModel = apply("coder", d.cfg.CoderModel)
	d.cfg.ReviewerModel = apply("reviewer", d.cfg.ReviewerModel)
}

// resolveAgentModel（S8.2 agent 级模型）：解析角色实际执行 agent 在 board「Agent 管理」
// 页绑定的模型档案（agents_registry.config.model_profile，按 agent 名存），命中则返回
// 档案（provider = 受管 provider "model-managed"，模型同步时已写入两侧框架的目录）。
// 优先级：agent 绑定 > active 档案（refreshModelsFromProfile）> env 默认。
// 档案缺失（已删）/ 无绑定 = nil（调用方回退上层配置）。
func (d *driver) resolveAgentModel(role, envFallback string) *db.ModelProfile {
	agent, err := d.agent(role, envFallback)
	if err != nil {
		return nil // agent 未注册：runCoder/runReviewer 会报同样的错，这里不重复
	}
	profileName := approval.AgentModelProfile(agent.Config)
	if profileName == "" {
		return nil
	}
	var p db.ModelProfile
	if err := d.db.Where("name = ?", profileName).First(&p).Error; err != nil {
		log.Printf("WARN: model-config: agent %s 绑定的档案 %q 不存在，回退全局配置", agent.Name, profileName)
		return nil
	}
	if p.Model == "" {
		return nil
	}
	return &p
}

func (d *driver) runAgentHost(adapterPath string, stdin []byte, extraEnv []string, timeout time.Duration) (string, string, error) {
	return d.runAgentHostCtx(d.ctx(), adapterPath, stdin, extraEnv, timeout)
}

// runAgentHostCtx 与 runAgentHost 相同，但允许外部 context 提前取消（用于「任务已被外部完成就杀掉 reviewer」）。
func (d *driver) runAgentHostCtx(parent context.Context, adapterPath string, stdin []byte, extraEnv []string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, adapterPath)
	// 进程组（2026-11-03）：adapter 会再派生 node / opencodereview 等孙进程。
	// 默认的 CommandContext 只 SIGKILL 直接子进程 → 父进程死了孙进程变孤儿继续跑，
	// 占着 CPU/模型配额（实测：任务已合入，ocr review 还在继续）。
	// 所以把自己放进新进程组，取消时按进程组整体杀。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负 pid = 整个进程组
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	// ctx 取消后最多等 5s 回收，避免 Run 挂住
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if parent.Err() != nil {
		// 上级 context 被取消（任务已被外部完成）→ 不是超时，也不是 agent 失败
		return stdout.String(), stderr.String(), errTaskFinishedExternally
	}
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), stderr.String(), fmt.Errorf("timeout after %v", timeout)
	}
	return stdout.String(), stderr.String(), err
}

// hostAgentEnv 组装宿主执行 agent 的 PATH + HOME（宿主 npm/go/cargo 工具链）。
func (d *driver) hostAgentEnv(hostHome string) []string {
	path := hostHome + "/.npm-global/bin:" +
		hostHome + "/Soft/go/bin:" +
		hostHome + "/.cargo/bin:" +
		"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	return []string{
		"HOME=" + hostHome,
		"PATH=" + path,
	}
}

// runCoder 调 coder adapter（容器内直接 exec，宿主工具链经 /host 挂载）。
func (d *driver) runCoder(task *db.EvolveTask, feedback, sessionID string) (*coderOut, error) {
	return d.runCoderCtx(d.ctx(), task, feedback, sessionID)
}

// runCoderCtx 同 runCoder，但可被 ctx 提前取消（任务已被外部完成 → 杀掉 coder 进程组）。
func (d *driver) runCoderCtx(ctx context.Context, task *db.EvolveTask, feedback, sessionID string) (*coderOut, error) {
	agent, err := d.agent("coder", d.cfg.AgentBindingCoder)
	if err != nil {
		return nil, err
	}
	ev := parseEvidence(task.Evidence)
	workdir := d.cfg.RepoDir + "/" + d.cfg.EvolveCacheDir + "/" + task.TaskNo
	input := map[string]any{
		"op":         "fix",
		"task":       taskText(ev), // 完整任务描述：仅首轮（新建会话）使用
		"feedback":   feedback,     // 上一轮反馈（empty diff / reviewer error）：仅续接会话时单独发送
		"workdir":    workdir,
		"files":      ev["files"],
		"kind":       ev["kind"], // bug | requirement：adapter 据此切换提示词（bug 快速定位 / 需求充分探索）
		"session_id": sessionID,  // 非空时 adapter 用 pi --session-id 续接会话（多轮对话）
	}
	b, _ := json.Marshal(input)

	provider, model := d.cfg.CoderProvider, d.cfg.CoderModel
	if p := d.resolveAgentModel("coder", d.cfg.AgentBindingCoder); p != nil {
		provider, model = "model-managed", p.Model
		log.Printf("model-config: coder agent → %q（档案 %q）", p.Model, p.Name)
	}
	env := d.hostAgentEnv(d.cfg.CoderHostHome)
	env = append(env,
		"PI_WEB_URL="+d.cfg.CoderWebURL,
		"PI_WEB_PASSWORD="+d.cfg.CoderWebPassword,
		"PI_PROVIDER="+provider,
		"PI_MODEL="+model,
		// pi-web 宿主运行时看不到容器内 /repo，adapter 据此把 cwd 翻译成宿主仓库根
		"PI_HOST_REPO="+d.cfg.HostRepo,
	)

	stdout, stderr, err := d.runAgentHostCtx(ctx, d.cfg.CoderAdapterPath, b, env, agentTimeout(agent))
	stdOut := strings.TrimSpace(stdout)
	if err != nil {
		// adapter fail() 把 {"ok":false,"reason":"..."} 写 stdout → 优先取 reason
		if reason := extractAdapterReason(stdOut); reason != "" {
			return nil, fmt.Errorf("exec %s: %s", agent.Name, reason)
		}
		return nil, fmt.Errorf("exec %s: %v: stderr=%s", agent.Name, err, tail(stderr, 300))
	}
	if stdOut == "" {
		return nil, fmt.Errorf("empty output: stderr=%s", tail(stderr, 300))
	}
	// v6 Validate 收解析后的 JSON 值（传 json.RawMessage 会报 invalid jsonType []uint8）
	var data any
	if err := json.Unmarshal([]byte(stdOut), &data); err != nil {
		return nil, fmt.Errorf("output not JSON (协议漂移): %s", tail(stdOut, 200))
	}
	if err := coderSchema.Validate(data); err != nil {
		return nil, fmt.Errorf("output schema 校验失败（协议漂移）: %v", err)
	}
	var out coderOut
	if err := json.Unmarshal([]byte(stdOut), &out); err != nil {
		return nil, fmt.Errorf("output not JSON (协议漂移): %s", tail(stdOut, 200))
	}
	if !out.OK {
		return nil, fmt.Errorf("coder ok=false: %s", tail(stdOut, 300))
	}
	d.store.Log(d.ctx(), "evolve-driver", "coder_done", task.TaskNo, map[string]any{
		"agent": agent.Name, "session_id": out.SessionID, "files": out.FilesChanged,
	})
	return &out, nil
}

// reviewerRetryBackoffs 返回 reviewer 执行失败后的重试退避序列（长度 = retries，指数翻倍）。
// base <= 0 时回退到 15s；retries <= 0 返回空（不重试）。
func reviewerRetryBackoffs(retries int, base time.Duration) []time.Duration {
	if retries <= 0 {
		return nil
	}
	if base <= 0 {
		base = 15 * time.Second
	}
	out := make([]time.Duration, 0, retries)
	d := base
	for i := 0; i < retries; i++ {
		out = append(out, d)
		d *= 2
	}
	return out
}

// reviewExcludePatterns 返回 reviewer 排除模式（内置默认表 + apps/<app>/.aiflow 的 review_ignore）。
func (d *driver) reviewExcludePatterns() []string {
	return agentcli.ReviewExcludePatterns(d.reviewIgnoreFromAIFlow())
}

// reviewIgnoreFromAIFlow 扫描 apps/*/.aiflow 的 review_ignore（相对 apps/<app>/，逗号分隔），
// 拼成仓库相对路径（与 diff / reviewer 评论路径同口径）。
// 从主仓库（RepoDir）读，不读任务 worktree：策略文件提交后立即生效，不用等新任务。
func (d *driver) reviewIgnoreFromAIFlow() []string {
	files, _ := filepath.Glob(filepath.Join(d.cfg.RepoDir, "apps", "*", ".aiflow"))
	out := make([]string, 0, len(files))
	for _, f := range files {
		app := filepath.Base(filepath.Dir(f))
		kv := parseAIFlowFile(f)
		for _, p := range strings.Split(kv["review_ignore"], ",") {
			if p = prefixAIFlowReviewPattern(app, p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// prefixAIFlowReviewPattern 把 .aiflow review_ignore 条目归一为仓库相对路径：
// 已带 apps/ 前缀原样；以 / 开头视为仓库根绝对路径；其余按 apps/<app>/ 相对路径拼接。
func prefixAIFlowReviewPattern(app, pattern string) string {
	p := strings.TrimSpace(pattern)
	switch {
	case p == "":
		return ""
	case strings.HasPrefix(p, "apps/"):
		return p
	case strings.HasPrefix(p, "/"):
		return strings.TrimPrefix(p, "/")
	default:
		return "apps/" + app + "/" + p
	}
}

// filterGeneratedComments 剔除落在生成产物上的评论（lock 文件 / 构建输出 / ABI 绑定 / .env 等）。
// 这些文件没有可“修”的语义，参与打回只会让任务在重修轮里死循环（2026-10-28 实测）。
func filterGeneratedComments(in []reviewComment, patterns []string) ([]reviewComment, int) {
	kept := make([]reviewComment, 0, len(in))
	ignored := 0
	for _, c := range in {
		if c.File != "" && agentcli.ReviewExcludedWithPatterns(c.File, patterns) {
			ignored++
			continue
		}
		kept = append(kept, c)
	}
	return kept, ignored
}

// runReviewerWithRetry 调 reviewer；**执行类失败**（provider/HTTP/超时/空输出/schema 漂移/ok=false）
// 按指数退避重试（REVIEWER_RETRIES，默认 3 次：15s → 30s → 60s）。
// 审核结论（level=error 评论）不在重试范围——那是业务判断，由调用方反馈 coder 重改。
// runReviewerWithRetry 跑 reviewer；失败按退避重试，耗尽后返回错误（调用方转人工）。
//
// **跳过审核**（2026-11-03）：task.SkipReview 置位时直接返回「无评论」，
// 主流程自然继续到沙箱/CI → 审批 → 看板 Review 列（人工看 diff）。
//
// 为什么在入口就返回而不是在主流程里分支：reviewer 阶段嵌在 coder 循环里，
// 一旦判废就 continue/return，改成外部开关会打乱轮次语义；
// 在这里返回空结果，等价于「reviwer 认为没问题」，后续阶段完全不用改。
func (d *driver) runReviewerWithRetry(task *db.EvolveTask, out *coderOut) (*reviewerOut, string, error) {
	if d.skipReviewRequested(task.TaskNo) {
		reason := `{"ok":true,"skipped":true,"reason":"skip_review（人工要求跳过 AI 审核，直接进 Review 列）"}`
		d.store.Log(d.ctx(), "evolve-driver", "review_skipped", task.TaskNo, map[string]any{
			"agent": "ocr", "reason": "skip_review flag set by human",
		})
		log.Printf("evolve %s: 已标记跳过审核 → 越过 reviewer，继续沙箱/CI（人工在 Review 列看 diff）", task.TaskNo)
		return &reviewerOut{}, reason, nil
	}
	backoffs := reviewerRetryBackoffs(d.cfg.ReviewerRetries, time.Duration(d.cfg.ReviewerRetryBackoffSec)*time.Second)
	// 任务被外部完成（审批合入 / 人工结束）时自动取消 reviewer，释放串行队列
	ctx, cancel := d.watchTaskFinished(task.TaskNo)
	defer cancel()
	var lastErr error
	for i := 0; i <= len(backoffs); i++ {
		if ctx.Err() != nil {
			return nil, "", errTaskFinishedExternally
		}
		rev, raw, err := d.runReviewerCtx(ctx, task, out)
		if err == nil {
			return rev, raw, nil
		}
		if errors.Is(err, errTaskFinishedExternally) {
			return nil, "", errTaskFinishedExternally
		}
		lastErr = err
		if i == len(backoffs) {
			break // 重试耗尽
		}
		wait := backoffs[i]
		log.Printf("evolve %s: reviewer 执行失败，%s 后重试（%d/%d）：%v",
			task.TaskNo, wait, i+1, len(backoffs), err)
		d.store.Log(d.ctx(), "evolve-driver", "reviewer_retry", task.TaskNo, map[string]any{
			"attempt": i + 1, "max_retries": len(backoffs),
			"backoff_sec": int(wait.Seconds()), "error": tail(err.Error(), 200),
		})
		time.Sleep(wait)
	}
	return nil, "", lastErr
}

// runReviewer 调 reviewer adapter（容器内直接 exec，宿主工具链经 /host 挂载）。
func (d *driver) runReviewer(task *db.EvolveTask, out *coderOut) (*reviewerOut, string, error) {
	return d.runReviewerCtx(d.ctx(), task, out)
}

// runReviewerCtx 同 runReviewer，但可被 ctx 提前取消（任务已被外部完成 → 杀掉 ocr）。
func (d *driver) runReviewerCtx(ctx context.Context, task *db.EvolveTask, out *coderOut) (*reviewerOut, string, error) {
	agent, err := d.agent("reviewer", d.cfg.AgentBindingReviewer)
	if err != nil {
		return nil, "", err
	}
	workdir := d.cfg.RepoDir + "/" + d.cfg.EvolveCacheDir + "/" + task.TaskNo
	input := map[string]any{
		"op":         "review",
		"diff":       out.Diff,
		"workdir":    workdir,
		"base":       d.cfg.BaseBranch,
		"start_unix": time.Now().Unix(), // 适配器据此识别本次评审产生的 ocr 会话
	}
	b, _ := json.Marshal(input)

	provider, model := d.cfg.ReviewerProvider, d.cfg.ReviewerModel
	if p := d.resolveAgentModel("reviewer", d.cfg.AgentBindingReviewer); p != nil {
		provider, model = "model-managed", p.Model
		log.Printf("model-config: reviewer agent → %q（档案 %q）", p.Model, p.Name)
	}
	env := d.hostAgentEnv(d.cfg.ReviewerHostHome)
	env = append(env,
		"OCR_BIN="+d.cfg.ReviewerHostHome+"/.npm-global/bin/ocr",
		"OCR_PROVIDER="+provider,
		"OCR_MODEL="+model,
	)
	// 项目特有生成物：apps/<app>/.aiflow 的 review_ignore（追加到 agentcli 内置默认表）
	if extra := d.reviewIgnoreFromAIFlow(); len(extra) > 0 {
		env = append(env, "OCR_EXCLUDE="+strings.Join(extra, ","))
	}

	stdout, stderr, err := d.runAgentHostCtx(ctx, d.cfg.ReviewerAdapterPath, b, env, agentTimeout(agent))
	stdOut := strings.TrimSpace(stdout)
	if err != nil {
		if reason := extractAdapterReason(stdOut); reason != "" {
			return nil, "", fmt.Errorf("exec %s: %s", agent.Name, reason)
		}
		return nil, "", fmt.Errorf("exec %s: %v: stderr=%s", agent.Name, err, tail(stderr, 300))
	}
	if stdOut == "" {
		return nil, "", fmt.Errorf("empty output: stderr=%s", tail(stderr, 300))
	}
	var data any
	if err := json.Unmarshal([]byte(stdOut), &data); err != nil {
		return nil, "", fmt.Errorf("output not JSON: %s", tail(stdOut, 200))
	}
	if err := reviewerSchema.Validate(data); err != nil {
		return nil, "", fmt.Errorf("output schema 校验失败（协议漂移）: %v", err)
	}
	var out2 struct {
		OK       bool            `json:"ok"`
		Comments []reviewComment `json:"comments"`
		Sessions []string        `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdOut), &out2); err != nil {
		return nil, "", fmt.Errorf("output not JSON: %s", tail(stdOut, 200))
	}
	if !out2.OK {
		return nil, "", fmt.Errorf("reviewer ok=false: %s", tail(stdOut, 300))
	}
	// review 存本轮完整记录：执行记录里每条「审核完成」事件都能点开看当轮评论
	// （task.Review 只保留最后一轮，多轮重修时前几轮的审核意见会丢）。
	// sessions = 本次评审产生的 ocr 会话 ID（board 侧据此拼出会话文件链接，看审核过程）。
	d.store.Log(d.ctx(), "evolve-driver", "reviewer_done", task.TaskNo, map[string]any{
		"agent": agent.Name, "comments": len(out2.Comments), "review": stdOut, "sessions": out2.Sessions,
	})
	return &reviewerOut{OK: out2.OK, Comments: out2.Comments}, stdOut, nil
}

// scopeCheck 是写范围校验结果：Violation 非空 = 超阈值打回；Allowed 非空 = 小范围越界已放行（留痕用）。
type scopeCheck struct {
	Violation    string
	Allowed      []string
	AllowedLines int
}

// checkScope 写范围校验：diff 触及文件默认应在 apps/* 下（排除 apps/board* 与 apps/core*：
// board 已拆 4 服务（board-content/board-email + core-ui 双入口）+ core 为 Go 核心，均人工维护）；
// **小范围越界允许**（2026-10-08）：越界文件数 ≤ EVOLVE_SCOPE_ALLOW_FILES
// 且越界改动行数 ≤ EVOLVE_SCOPE_ALLOW_LINES → 放行（仍经 reviewer + 人工审批双重把关，放行写 audit 留痕）；
// 超阈值/行数无法判定（diff 为空）→ 打回人工 PR。
// checkScope 写范围校验：diff 触及文件默认应在 apps/* 下（排除 apps/board* 与 apps/core*：
// core = Go 核心、core-ui = board 前端，人工维护只走人工 PR）；
// **任务显式选择的目标**（target_name 多值，如 core-ui / core / dapp）视为已授权，放行对应 apps/<name>/ 目录
// （选择即授权；reviewer + 人工审批仍把关）；
// **小范围越界允许**（2026-10-08）：越界文件数 ≤ EVOLVE_SCOPE_ALLOW_FILES
// 且越界改动行数 ≤ EVOLVE_SCOPE_ALLOW_LINES → 放行（仍经 reviewer + 人工审批双重把关，放行写 audit 留痕）；
// 超阈值/行数无法判定（diff 为空）→ 打回人工 PR。
func (d *driver) checkScope(task *db.EvolveTask, out *coderOut) scopeCheck {
	bad := outOfBoundsFiles(collectChangedFiles(out), splitTargets(task.TargetName))
	if len(bad) == 0 {
		return scopeCheck{}
	}
	lines := countDiffChangedLines(out.Diff, bad)
	if d.cfg.ScopeAllowFiles > 0 && d.cfg.ScopeAllowLines > 0 &&
		len(bad) <= d.cfg.ScopeAllowFiles && lines > 0 && lines <= d.cfg.ScopeAllowLines {
		return scopeCheck{Allowed: bad, AllowedLines: lines}
	}
	return scopeCheck{Violation: fmt.Sprintf("越界文件 %d 个/改动 %d 行（阈值 %d 文件/%d 行）: %s",
		len(bad), lines, d.cfg.ScopeAllowFiles, d.cfg.ScopeAllowLines, strings.Join(bad, ", "))}
}

// collectChangedFiles 收集 coder 改动文件（files_changed + diff 头兜底），去重。
// collectChangedFiles 收集 coder 改动文件（files_changed + diff 头兜底），去重；路径统一归一化
// （子仓库前缀拼接曾产生 apps/dapp//src/... 双斜杠，导致 scope 误判越界）。
func collectChangedFiles(out *coderOut) []string {
	files := make([]string, 0, len(out.FilesChanged))
	for _, f := range out.FilesChanged {
		files = append(files, normalizeRepoPath(f))
	}
	// 兜底：从 diff 头解析（adapter 未报 files_changed 时）
	for _, line := range strings.Split(out.Diff, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			files = append(files, normalizeRepoPath(strings.TrimPrefix(line, "+++ b/")))
		}
	}
	return dedupe(files)
}

// normalizeRepoPath 归一化 diff / 文件列表路径：去首尾空白、去 ./ 前缀、折叠重复斜杠。
func normalizeRepoPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// splitTargets 拆分多值 target_name（逗号分隔，去空白）。
func splitTargets(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// outOfBoundsFiles 筛出越界文件：非 apps/* 或 apps/board* 或 apps/core*（core = Go 核心，
// core-ui = 人工维护的 board 前端，均只走人工 PR）。
// outOfBoundsFiles 筛出越界文件：非 apps/* 一律越界；apps/board* / apps/core* 默认越界
// （core = Go 核心、core-ui = board 前端，人工维护只走人工 PR），
// 但**任务显式选中的目标**（targets，如 core-ui / core）放行对应 apps/<name>/ 目录（选择即授权）。
func outOfBoundsFiles(files []string, targets []string) []string {
	explicit := map[string]bool{}
	for _, t := range targets {
		explicit[strings.TrimSpace(t)] = true
	}
	var bad []string
	for _, f := range files {
		f = normalizeRepoPath(f)
		if f == "" {
			continue
		}
		if !strings.HasPrefix(f, "apps/") {
			bad = append(bad, f)
			continue
		}
		if !strings.HasPrefix(f, "apps/board") && !strings.HasPrefix(f, "apps/core") {
			continue // 普通 apps/* 目录：默认放行
		}
		// 受保护目录（board*/core*）：仅当任务显式把该 app 列为目标时放行
		appName := strings.SplitN(strings.TrimPrefix(f, "apps/"), "/", 2)[0]
		if explicit[appName] {
			continue
		}
		bad = append(bad, f)
	}
	return bad
}

// countDiffChangedLines 统计 unified diff 中属于指定文件的改动行数（+/- 行，不含文件头）。
func countDiffChangedLines(diff string, files []string) int {
	want := map[string]bool{}
	for _, f := range files {
		want[f] = true
	}
	cur := ""
	count := 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			cur = "" // 新文件段落，等 +++ 头确认路径
		case strings.HasPrefix(line, "+++ b/"):
			cur = normalizeRepoPath(strings.TrimPrefix(line, "+++ b/"))
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			cur = "" // /dev/null 等非 b/ 头
		case (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) && cur != "" && want[cur]:
			count++
		}
	}
	return count
}

// sandboxResult 是 sandbox-runner 响应。
