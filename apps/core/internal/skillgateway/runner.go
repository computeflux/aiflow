package skillgateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/circuitbreaker"
	"github.com/wetee/ai-workflow/internal/db"
	internalotel "github.com/wetee/ai-workflow/internal/otel"
	"go.opentelemetry.io/otel/attribute"
)

// skill_runs.status 取值。
const (
	RunStatusOK       = "ok"
	RunStatusFail     = "fail"
	RunStatusTimeout  = "timeout"
	RunStatusRejected = "rejected"
)

// RunMeta 是从 HTTP 头提取的调用上下文（由 Temporal 桥接 activity 传入）。
type RunMeta struct {
	WorkflowID string
	RunID      string
}

// InvokeResult 是一次调用的最终结果。Output 是「skill 原始输出 + 注入的
// skill/version/trace_id 元信息」的扁平 map，直接作为 HTTP 200 响应体。
type InvokeResult struct {
	Output map[string]any
}

// InvokeError 携带 HTTP 状态码的调用错误。
// 状态码映射见 docs/plan/transform-plan.md §6.4：
// 400 非法输入 / 404 skill 不存在 / 429 限流 / 502 执行失败 / 503 无 live 版。
type InvokeError struct {
	Code   int
	Err    error
	Output map[string]any // 502 时附带 skill 的原始输出，便于排障
}

func (e *InvokeError) Error() string { return e.Err.Error() }
func (e *InvokeError) Unwrap() error { return e.Err }

// Runner 执行核心调用链：熔断检查 → 校验输入 → 限流 → 执行 → 校验输出 → 写 skill_runs。
type Runner struct {
	reg *Registry
	lim *limiterStore
	// S3 §3.1 熔断器（可 nil = 未启用）：每个 skill 一个实例，状态持久化 skill_circuit。
	brk    *circuitbreaker.Registry
	onOpen func(name string) // breaker 刚 open 时回调（写 intervention + 告警）
}

// NewRunner 创建 Runner。
func NewRunner(reg *Registry) *Runner {
	return &Runner{reg: reg, lim: newLimiterStore()}
}

// WithBreaker 挂接熔断器（S3 §3.1）；onOpen 在 breaker 首次 open 时调用
// （cmd/api 接线：写 interventions kind=circuit_open + 告警）。
func (r *Runner) WithBreaker(brk *circuitbreaker.Registry, onOpen func(name string)) *Runner {
	r.brk = brk
	r.onOpen = onOpen
	return r
}

// Invoke 执行一个 skill 调用并写 skill_runs（每个调用只写一条最终记录）。
//
// 返回的 InvokeResult.Output 已包含 skill 原始输出 + skill/version/trace_id。
// 任何失败路径都会先落库（status=rejected/fail/timeout）再返回 InvokeError。
func (r *Runner) Invoke(ctx context.Context, name string, input map[string]any, meta RunMeta) (*InvokeResult, error) {
	start := time.Now()

	skill, version, err := r.reg.ResolveLive(ctx, name)
	if err != nil {
		switch {
		case errors.Is(err, ErrSkillNotFound):
			return nil, &InvokeError{Code: http.StatusNotFound, Err: err}
		case errors.Is(err, ErrNoLiveVersion):
			return nil, &InvokeError{Code: http.StatusServiceUnavailable, Err: err}
		default:
			return nil, &InvokeError{Code: http.StatusInternalServerError, Err: err}
		}
	}

	// 开 span：所有 skill_runs 记录（含 rejected）都带同一个 trace id，
	// 与调用方（Temporal workflow / board 手动调用）串成一条链路。
	spanCtx, span := internalotel.Start(ctx, "skill.invoke",
		attribute.String("skill.name", name),
		attribute.String("skill.version", version.Version),
	)
	defer span.End()
	traceID := internalotel.TraceIDFrom(spanCtx)
	if traceID == "" {
		traceID = randomHex32()
	}

	limits := parseLimits(skill.Limits)
	inputHash := hashInput(input)

	// 0. 熔断器（S3 §3.1）：open 直接 503，不执行、不写 skill_runs
	//    （拒绝原因由 interventions + 告警留痕；半开探测放行）
	if r.brk != nil {
		br := r.brk.Get(name)
		if !br.Allow(ctx) {
			return nil, &InvokeError{Code: http.StatusServiceUnavailable, Err: fmt.Errorf("circuit open for skill %s", name)}
		}
	}

	// 1. 校验输入 → 400（记 rejected）
	if err := validateAgainst(schemaURLInput, json.RawMessage(skill.InputSchema), input); err != nil {
		_ = r.recordRun(ctx, &db.SkillRun{
			SkillName:  name,
			Version:    version.Version,
			TraceID:    traceID,
			WorkflowID: meta.WorkflowID,
			RunID:      meta.RunID,
			InputHash:  inputHash,
			Status:     RunStatusRejected,
			LatencyMs:  elapsedMs(start),
			Error:      truncateForError(err.Error()),
			CreatedAt:  time.Now(),
		})
		return nil, &InvokeError{Code: http.StatusBadRequest, Err: fmt.Errorf("invalid input: %w", err)}
	}

	// 2. 限流 + 并发控制 → 429（记 rejected）
	entry := r.lim.entry(name, limits)
	release, err := entry.acquire()
	if err != nil {
		_ = r.recordRun(ctx, &db.SkillRun{
			SkillName:  name,
			Version:    version.Version,
			TraceID:    traceID,
			WorkflowID: meta.WorkflowID,
			RunID:      meta.RunID,
			InputHash:  inputHash,
			Status:     RunStatusRejected,
			LatencyMs:  elapsedMs(start),
			Error:      err.Error(),
			CreatedAt:  time.Now(),
		})
		return nil, &InvokeError{Code: http.StatusTooManyRequests, Err: err}
	}
	defer release()

	// 3. 执行（带 limits.timeout_sec 超时）
	execCtx, cancel := ensureTimeoutCtx(spanCtx, limits)
	defer cancel()

	spec := skillSpecFromRow(skill, limits)
	// S5 §3.4 灰度路由：版本实例的 addr 在 skill_versions.config 快照里
	//（deployctl version up 写入；skills.addr 保持基线服务不动）——
	// 网关按 traffic_split 分流到旧/新版本实例各自的 addr。
	if addr := VersionConfigAddr(version.Config); addr != "" {
		spec.Addr = addr
	}
	out, execErr := invoke(execCtx, spec, input)
	if execErr != nil {
		status := RunStatusFail
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			status = RunStatusTimeout
		}
		_ = r.recordRun(ctx, &db.SkillRun{
			SkillName:  name,
			Version:    version.Version,
			TraceID:    traceID,
			WorkflowID: meta.WorkflowID,
			RunID:      meta.RunID,
			InputHash:  inputHash,
			Status:     status,
			LatencyMs:  elapsedMs(start),
			Error:      truncateForError(execErr.Error()),
			CreatedAt:  time.Now(),
		})
		r.recordBreakerFailure(ctx, name)
		return nil, &InvokeError{Code: http.StatusBadGateway, Err: execErr}
	}

	// 4. 校验输出：schema + 顶层 ok 字段
	if err := validateAgainst(schemaURLOutput, json.RawMessage(skill.OutputSchema), out); err != nil {
		_ = r.recordRun(ctx, &db.SkillRun{
			SkillName:  name,
			Version:    version.Version,
			TraceID:    traceID,
			WorkflowID: meta.WorkflowID,
			RunID:      meta.RunID,
			InputHash:  inputHash,
			Status:     RunStatusFail,
			LatencyMs:  elapsedMs(start),
			Error:      truncateForError(fmt.Sprintf("invalid output: %v", err)),
			CreatedAt:  time.Now(),
		})
		r.recordBreakerFailure(ctx, name)
		return nil, &InvokeError{Code: http.StatusBadGateway, Err: fmt.Errorf("invalid output: %w", err), Output: out}
	}
	okVal, okBool := out["ok"].(bool)
	if !okBool {
		_ = r.recordRun(ctx, &db.SkillRun{
			SkillName:  name,
			Version:    version.Version,
			TraceID:    traceID,
			WorkflowID: meta.WorkflowID,
			RunID:      meta.RunID,
			InputHash:  inputHash,
			Status:     RunStatusFail,
			LatencyMs:  elapsedMs(start),
			Error:      "output missing boolean field 'ok'",
			CreatedAt:  time.Now(),
		})
		r.recordBreakerFailure(ctx, name)
		return nil, &InvokeError{Code: http.StatusBadGateway, Err: errors.New("output missing boolean field 'ok'"), Output: out}
	}

	reason := ""
	if !okVal {
		reason = fmt.Sprintf("%v", out["reason"])
	}
	_ = r.recordRun(ctx, &db.SkillRun{
		SkillName:  name,
		Version:    version.Version,
		TraceID:    traceID,
		WorkflowID: meta.WorkflowID,
		RunID:      meta.RunID,
		InputHash:  inputHash,
		Status:     RunStatusOK,
		LatencyMs:  elapsedMs(start),
		Error:      reason,
		CreatedAt:  time.Now(),
	})
	if r.brk != nil {
		if okVal {
			r.brk.Get(name).RecordSuccess(ctx)
		} else {
			r.recordBreakerFailure(ctx, name)
		}
	}
	if !okVal {
		return nil, &InvokeError{
			Code:   http.StatusBadGateway,
			Err:    fmt.Errorf("skill reported failure: %s", reason),
			Output: out,
		}
	}

	// 5. 成功：注入元信息后返回
	out["skill"] = name
	out["version"] = version.Version
	out["trace_id"] = traceID
	return &InvokeResult{Output: out}, nil
}

// invoke 按 skill type 分派执行（rt = transport 注入点，nil = serviceTransport / 默认）。
//
// MIG-1（工作项 wi-1795853890）：addr 指向 social 时走 invokeSocial —— social 是多渠道
// 服务，x-scan / like 挂在 /api/social/{channel}/{action} 上，没有 pycraper / pyreplay
// 那种「每进程一个 skill 的 /invoke」端点，直接拼 /invoke 只会 404（切流一放量就断流）。
func invoke(ctx context.Context, spec *SkillSpec, input map[string]any, rt ...http.RoundTripper) (map[string]any, error) {
	if spec.Type == "service" {
		var transport http.RoundTripper
		if len(rt) > 0 {
			transport = rt[0]
		}
		if addr := socialBaseOf(spec.Addr); addr != "" {
			return invokeSocialWithTransport(ctx, transport, spec.Name, addr, input)
		}
		return invokeService(ctx, spec, input, transport)
	}
	return invokeCLI(ctx, spec, input)
}

// recordRun 落库失败只记录日志级别的错误？——不，这里直接忽略错误：
// 执行结果本身比落库失败更重要；落库失败会在 error_logs 由上层捕获。
func (r *Runner) recordRun(ctx context.Context, run *db.SkillRun) error {
	return r.reg.RecordRun(ctx, run)
}

// recordBreakerFailure 记录一次失败调用；breaker 刚 open 时触发 onOpen 回调
// （cmd/api 接线：写 interventions kind=circuit_open + 告警）。
func (r *Runner) recordBreakerFailure(ctx context.Context, name string) {
	if r.brk == nil {
		return
	}
	if opened := r.brk.Get(name).RecordFailure(ctx); opened && r.onOpen != nil {
		r.onOpen(name)
	}
}

// skillSpecFromRow 从 skills 行重建执行所需 spec（schema 无需带回）。
func skillSpecFromRow(skill *db.Skill, limits Limits) *SkillSpec {
	return &SkillSpec{
		Name:       skill.Name,
		Version:    "",
		Owner:      skill.Owner,
		Runtime:    skill.Runtime,
		Type:       skill.Type,
		Entrypoint: skill.Entrypoint,
		Addr:       skill.Addr,
		Container:  skill.Container,
		Limits:     limits,
	}
}

// parseLimits 解析 skills.limits JSON 字符串；失败时返回默认值（不阻断调用）。
func parseLimits(raw string) Limits {
	l := Limits{}
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return Limits{}.withDefaults()
	}
	return l.withDefaults()
}

// hashInput 计算输入的 sha256 hex（64 字符）。encoding/json 对 map key 排序，
// 因此相同语义输入（不同 key 顺序）哈希一致。
func hashInput(input map[string]any) string {
	b, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// elapsedMs 计算距 start 的毫秒数。
func elapsedMs(start time.Time) *int64 {
	ms := time.Since(start).Milliseconds()
	return &ms
}

// randomHex32 生成 32 位随机 hex（无 trace context 时的兜底 trace id）。
func randomHex32() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", 32)
	}
	return hex.EncodeToString(b)
}
