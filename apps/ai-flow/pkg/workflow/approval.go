// [workflow-fork] P4-1: ai-flow 写操作两阶段确认（preview → confirm）+ audit_logs 双写
// (docs/plan/ai-bot-picoclaw-fork-plan.md §1.3 / §4，工作项 wi-1794237813-c731)
//
// 目标：workflow 写工具（pkg/tools/workflow/*）在真正调用 workflow-api 写接口
// 之前，必须经过一次人类确认。链路：
//
//	Preview: tool(confirm=false, args) → Approval.Preview()
//	          → 不落 API，返回 Preview{Action, ParamsHash, ConfirmToken, ExpiresAt}
//	          （confirm_token = HMAC-SHA256(secret, user_id|tool|params_hash|expire)）
//	Confirm: tool(confirm=true, token) → Approval.Confirm()
//	          → 常量时间比较校验签名 / 过期 / 参数一致 / 单次使用（重放拒绝）
//	          → 放行真实 API 调用，随后写审计（audit_logs 双写）。
//
// 审计双写：Confirm 成功后调 AuditSink（默认 HTTPAuditSink → workflow-api
// POST /audit-logs，落 apps/core internal/approval 六表体系的 audit_logs，
// 只增不删）。字段 actor=ai-bot、on_behalf_of=<user_id>、tool、params_hash、
// result。写审计失败时高危操作（HighRisk）直接拒绝执行（返回错误，调用方
// 不得放行真实 API）。
//
// secret 走 .env PICOCLAW_APPROVAL_SECRET（与 pkg/workflow/config.go 的
// AIFLOW_* 约定一致，由 compose env_file / godotenv 注入进程环境）。
//
// 本文件不依赖 pkg/config：纯 stdlib + 既有 pkg/workflow.Client，便于单测。

package workflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EnvApprovalSecret is the env key carrying the HMAC signing secret for
// confirm tokens (PICOCLAW_APPROVAL_SECRET, root .env / compose env_file).
const EnvApprovalSecret = "PICOCLAW_APPROVAL_SECRET"

// DefaultApprovalTTL is the confirm-token lifetime (spec: 5 minutes).
const DefaultApprovalTTL = 5 * time.Minute

// Sentinel errors for the two-phase confirmation flow. Messages carry the
// user-facing explanation so a tool can return them to the model verbatim.
var (
	// ErrNoSecret: approval secret is not configured — preview/confirm are
	// unavailable, write tools must fail closed.
	ErrNoSecret = errors.New("approval unavailable: 两阶段确认未配置（缺少 PICOCLAW_APPROVAL_SECRET），写操作已被拒绝")
	// ErrNoToken: confirm=true without a confirm_token — 100% rejected, the
	// caller must run preview first (acceptance: 无 token 的写请求全部被拒).
	ErrNoToken = errors.New("approval required: 写操作需要两阶段确认，请先以 confirm=false 调用获取 preview 与 confirm_token，人类确认后再携带 token 重试")
	// ErrTokenExpired: the confirm token passed the signature check but its
	// expiry timestamp is in the past.
	ErrTokenExpired = errors.New("approval expired: 确认令牌已过期，请重新发起 preview 获取新的确认")
	// ErrTokenInvalid: HMAC verification failed (forged / foreign-secret /
	// tampered token) — constant-time comparison mismatch.
	ErrTokenInvalid = errors.New("approval denied: 确认令牌无效（签名校验失败），写操作已被拒绝")
	// ErrTokenReplayed: the token was already consumed — single-use is
	// enforced; a second confirm with the same token is rejected.
	ErrTokenReplayed = errors.New("approval used: 确认令牌已被使用（单次有效），如需再次执行请重新发起 preview")
	// ErrParamsChanged: confirm args do not hash to the params_hash bound
	// into the token — parameters were tampered with between preview and
	// confirm.
	ErrParamsChanged = errors.New("approval denied: 确认参数与 preview 时不一致（params_hash 不匹配），写操作已被拒绝")
	// ErrAuditWrite: the audit_logs double-write failed. High-risk operations
	// must be rejected when this error is returned (留痕红线).
	ErrAuditWrite = errors.New("approval denied: 审计写入失败（audit_logs 留痕是红线），高危操作已被拒绝")
)

// AuditSink writes one approval audit record to workflow-api (the
// apps/core internal/approval six-table system; audit_logs append-only).
type AuditSink interface {
	WriteAudit(ctx context.Context, rec AuditRecord) error
}

// AuditRecord is the audit double-write payload. Field names mirror the
// requirement: actor=ai-bot, on_behalf_of=<user_id>, tool, params_hash,
// result.
type AuditRecord struct {
	Actor      string `json:"actor"`        // always "ai-bot"
	OnBehalfOf string `json:"on_behalf_of"` // end-user id that triggered the action
	Tool       string `json:"tool"`         // tool name (the write tool)
	ParamsHash string `json:"params_hash"`  // SHA-256 of canonical confirm args
	Result     string `json:"result"`       // e.g. "confirmed_executed" | "preview_issued"
	Action     string `json:"action"`       // audit_logs.action, e.g. "picoclaw_write_confirm"
}

// HTTPAuditSink posts audit records to workflow-api POST /audit-logs using
// the shared pkg/workflow.Client (JWT from ctx is forwarded as Bearer).
type HTTPAuditSink struct {
	client *Client
}

// NewHTTPAuditSink creates an audit sink against the given workflow-api base
// URL (e.g. "http://127.0.0.1:8001" / WORKFLOW_API_URL).
func NewHTTPAuditSink(baseURL string) *HTTPAuditSink {
	c := NewClient(baseURL)
	// Audit double-write must be prompt but not block the tool loop: give
	// the confirm-path audit call a dedicated 5s budget (the client default
	// is tuned for read endpoints).
	c.SetTimeout(5 * time.Second)
	return &HTTPAuditSink{client: c}
}

// WriteAudit sends one audit record. The client applies its normal retry
// policy; the final error is returned so the caller can enforce the
// high-risk rejection rule.
func (s *HTTPAuditSink) WriteAudit(ctx context.Context, rec AuditRecord) error {
	return s.client.Do(ctx, "POST", "/audit-logs", nil, rec, nil)
}

// Preview is the human-readable preview returned when a write tool is
// invoked with confirm=false. The caller shows Action to the human; the
// token is handed back for the confirm phase.
type Preview struct {
	Tool       string    `json:"tool"`
	Action     string    `json:"action"`      // human-readable action description
	ParamsHash string    `json:"params_hash"` // sha256(canonical JSON args), hex
	Token      string    `json:"confirm_token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Decision is the result of a confirm verification. Allowed=true means the
// caller may perform the real API call (after the audit double-write
// succeeded — see Audit). Denied carries the reason for rejection.
type Decision struct {
	Allowed  bool
	HighRisk bool
	Audited  bool // audit double-write attempted
	Denied   error
	// TokenConsumed marks that this call consumed the token (the audit
	// failed afterwards and a high-risk op was refused): the same token
	// can never be replayed, the human must re-preview.
	TokenConsumed bool
	ParamsHash    string
}

// Approval implements the two-phase confirmation infrastructure. It is safe
// for concurrent use.
type Approval struct {
	secret []byte
	ttl    time.Duration
	sink   AuditSink
	now    func() time.Time // injectable clock for tests

	mu              sync.Mutex
	consumed        map[string]time.Time // consumed token digest → cleanup time
	pruneEvery      int
	callsSincePrune int
}

// NewApproval creates an approval broker. secret must not be empty (from
// PICOCLAW_APPROVAL_SECRET); ttl<=0 uses DefaultApprovalTTL; sink may be
// nil (audit disabled — in that case high-risk confirms are refused
// outright: fail closed, never a silent unaudited execution).
func NewApproval(secret string, ttl time.Duration, sink AuditSink) *Approval {
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	return &Approval{
		secret:   []byte(secret),
		ttl:      ttl,
		sink:     sink,
		now:      time.Now,
		consumed: map[string]time.Time{},
		// Prune consumed entries once per 64 issuances: consumed tokens are
		// kept for 2×TTL (well past any expiry check) then dropped, so the
		// replay map stays bounded.
		pruneEvery: 64,
	}
}

// NewApprovalBroker creates an approval broker with explicit clock and prune
// interval. Used by [workflow-fork] P4-2 write tools, which treat the
// broker as a shared stateless signer (preview issuance via Issued) and
// need a broker that works with an empty secret / zero-value construction
// (NewApproval leaves pruneEvery unset → Issued would panic on pruning).
func NewApprovalBroker(secret string, ttl time.Duration, sink AuditSink, now func() time.Time) *Approval {
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Approval{
		secret:     []byte(secret),
		ttl:        ttl,
		sink:       sink,
		now:        now,
		consumed:   map[string]time.Time{},
		pruneEvery: 64,
	}
}

// SetSink swaps the audit sink. ⚠️ 红线：仅限构造期/测试装配使用；
// 运行期请求路径绝不允许在共享 broker 上按请求切换 sink（Confirm 锁外读
// a.sink，并发切换会产生数据竞争与跨请求审计串扰）。需要按请求指定审计
// 目标时使用 ConfirmWithSink。 Nil restores the no-sink fail-closed behavior.
func (a *Approval) SetSink(sink AuditSink) {
	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
}

// Sink returns the configured audit sink under the broker lock (safe read
// of the construction-time sink).
func (a *Approval) Sink() AuditSink {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sink
}

// SetClock overrides the time source (tests). Nil restores time.Now.
func (a *Approval) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	a.now = now
}

// TTL returns the configured token lifetime.
func (a *Approval) TTL() time.Duration { return a.ttl }

// canonicalJSON marshals args to deterministic JSON: map keys sorted (the
// encoding/json marshaller already sorts map keys), no trailing newline.
func canonicalJSON(args any) ([]byte, error) {
	if args == nil {
		return []byte("null"), nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("approval: canonicalize args: %w", err)
	}
	return b, nil
}

// ParamsHash returns sha256(canonical JSON args) as lowercase hex.
func ParamsHash(args any) (string, error) {
	b, err := canonicalJSON(args)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// signingInput is the HMAC message: user_id|tool|params_hash|expire.
func signingInput(userID, tool, paramsHash string, expire int64) string {
	return fmt.Sprintf("%s|%s|%s|%d", userID, tool, paramsHash, expire)
}

// signRaw returns the raw HMAC-SHA256 digest bytes of the signing input.
func (a *Approval) signRaw(userID, tool, paramsHash string, expire int64) []byte {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(signingInput(strings.TrimSpace(userID), tool, paramsHash, expire)))
	return mac.Sum(nil)
}

// sign returns the hex-encoded signature (the token's sig part).
func (a *Approval) sign(userID, tool, paramsHash string, expire int64) string {
	return hex.EncodeToString(a.signRaw(userID, tool, paramsHash, expire))
}

// parseToken splits "<expire unix>|<hmac hex>"; ok=false on malformed input.
func parseToken(token string) (expire int64, sigHex string, ok bool) {
	sep := strings.LastIndexByte(token, '|')
	if sep <= 0 || sep == len(token)-1 {
		return 0, "", false
	}
	expireStr, sigStr := token[:sep], token[sep+1:]
	e, err := strconv.ParseInt(expireStr, 10, 64)
	if err != nil || e <= 0 {
		return 0, "", false
	}
	if len(sigStr) != sha256.Size*2 {
		return 0, "", false
	}
	if _, err := hex.DecodeString(sigStr); err != nil {
		return 0, "", false
	}
	return e, sigStr, true
}

// pruneLocked drops consumed entries older than 2×TTL (caller holds a.mu).
func (a *Approval) pruneLocked(now time.Time) {
	cutoff := now.Add(-2 * a.ttl)
	for k, t := range a.consumed {
		if t.Before(cutoff) {
			delete(a.consumed, k)
		}
	}
}

// Preview issues a preview + confirm token for a write-tool invocation
// (confirm=false). It never calls the real API. The token binds the
// identity (userID), tool name, and parameter hash, and expires after TTL.
func (a *Approval) Preview(userID, tool, action string, args any) (*Preview, error) {
	if len(a.secret) == 0 {
		return nil, ErrNoSecret
	}
	hash, err := ParamsHash(args)
	if err != nil {
		return nil, err
	}
	now := a.now()
	expire := now.Add(a.ttl).Unix()
	token := a.sign(userID, tool, hash, expire)

	a.mu.Lock()
	a.callsSincePrune++
	if a.callsSincePrune >= a.pruneEvery {
		a.pruneLocked(now)
		a.callsSincePrune = 0
	}
	a.mu.Unlock()

	return &Preview{
		Tool:       tool,
		Action:     action,
		ParamsHash: hash,
		Token:      fmt.Sprintf("%d|%s", expire, token),
		ExpiresAt:  time.Unix(expire, 0),
	}, nil
}

// Issued records a preview issuance in the replay-protection bookkeeping
// and returns a preview signed by this broker. It is identical to Preview()
// except that it never fails on a missing secret: the zero-value broker
// (no secret configured) issues an inert preview whose token can never pass
// Confirm (secretless Confirm is fail-closed with ErrNoSecret). [workflow-fork]
// P4-2: write tools keep the Approval broker as a shared stateless signer
// and perform their own preview→confirm flow, including preview issuance
// when no approval secret is configured (fail-closed at confirm time).
func (a *Approval) Issued(userID, tool, action string, args any) (*Preview, error) {
	hash, err := ParamsHash(args)
	if err != nil {
		return nil, err
	}
	now := a.now()
	expire := now.Add(a.ttl).Unix()
	token := a.sign(userID, tool, hash, expire)

	a.mu.Lock()
	a.callsSincePrune++
	if a.callsSincePrune >= a.pruneEvery {
		a.pruneLocked(now)
		a.callsSincePrune = 0
	}
	a.mu.Unlock()

	return &Preview{
		Tool:       tool,
		Action:     action,
		ParamsHash: hash,
		Token:      fmt.Sprintf("%d|%s", expire, token),
		ExpiresAt:  time.Unix(expire, 0),
	}, nil
}

// Confirm verifies a confirm token and, on success, consumes it (single
// use) and performs the audit double-write. The caller must only execute
// the real API call when Decision.Allowed is true.
//
// Rejections:
//   - empty token → ErrNoToken (无 token 的写请求 100% 被拒);
//   - malformed / forged token → ErrTokenInvalid (constant-time compare);
//   - expired → ErrTokenExpired;
//   - args hash mismatch → ParamsChanged → ErrParamsChanged;
//   - token already consumed (incl. canonicalized equivalent spellings)
//     → ErrTokenReplayed;
//   - high-risk + audit write failure (or no sink configured) →
//     ErrAuditWrite (refuse execution, fail closed).
func (a *Approval) Confirm(
	ctx context.Context,
	userID, tool string,
	args any,
	token string,
	highRisk bool,
	result string,
) (*Decision, error) {
	return a.confirm(ctx, userID, tool, args, token, highRisk, result, nil, false)
}

// ConfirmWithSink is Confirm with an explicit audit sink: the confirm-phase
// audit double-write goes to the passed sink, never to the broker's
// construction-time sink. [workflow-fork] P4-2: request paths that need a
// per-request audit target (e.g. base-URL override) MUST use this instead
// of mutating the shared broker's sink — the broker sink stays immutable at
// runtime. Pass nil to fall back to the broker's configured sink.
func (a *Approval) ConfirmWithSink(
	ctx context.Context,
	sink AuditSink,
	userID, tool string,
	args any,
	token string,
	highRisk bool,
	result string,
) (*Decision, error) {
	return a.confirm(ctx, userID, tool, args, token, highRisk, result, sink, true)
}

// confirm is the shared two-phase confirm implementation. When
// explicitSink is set (ConfirmWithSink), the audit double-write uses only
// the passed sink and never touches a.sink; otherwise the broker's
// construction-time sink is read once under the broker lock (no runtime
// mutation of a.sink on request paths — see SetSink).
func (a *Approval) confirm(
	ctx context.Context,
	userID, tool string,
	args any,
	token string,
	highRisk bool,
	result string,
	sink AuditSink,
	explicitSink bool,
) (*Decision, error) {
	if len(a.secret) == 0 {
		return nil, ErrNoSecret
	}
	token = trimToken(token)
	if token == "" {
		return &Decision{Denied: ErrNoToken}, ErrNoToken
	}

	now := a.now()
	expire, _, ok := parseToken(token)
	if !ok {
		return &Decision{Denied: ErrTokenInvalid}, ErrTokenInvalid
	}
	// Recompute the expected signature and compare in constant time.
	hash, err := ParamsHash(args)
	if err != nil {
		return nil, err
	}
	// Constant-time comparison of the presented sig part against the
	// expected digest (never early-exit on a byte-prefix match).
	presented, _ := hex.DecodeString(token[strings.LastIndexByte(token, '|')+1:])
	if !hmac.Equal(presented, a.signRaw(userID, tool, hash, expire)) {
		return &Decision{Denied: ErrTokenInvalid}, ErrTokenInvalid
	}
	// Signature valid → identity/tool/params are bound; check expiry.
	if !now.Before(time.Unix(expire, 0)) {
		return &Decision{Denied: ErrTokenExpired}, ErrTokenExpired
	}

	// Single-use: consume the token atomically (replay → reject). The key
	// is the CANONICAL (expire, sig) pair — parse-normalized decimal expire
	// + lowercase hex of the decoded digest — so equivalent spellings of a
	// consumed token ("01700000000|<sig>", upper-case sig hex, …) cannot
	// bypass the replay check. The consumed flag is set INSIDE the lock so
	// concurrent confirms of the same token race fairly: exactly one
	// proceeds.
	key := strconv.FormatInt(expire, 10) + "|" + hex.EncodeToString(presented)
	a.mu.Lock()
	if _, used := a.consumed[key]; used {
		a.mu.Unlock()
		return &Decision{Denied: ErrTokenReplayed, ParamsHash: hash}, ErrTokenReplayed
	}
	a.consumed[key] = now
	a.mu.Unlock()

	dec := &Decision{ParamsHash: hash, HighRisk: highRisk, TokenConsumed: true}

	// Audit double-write (actor=ai-bot, on_behalf_of=user). 留痕红线：高危
	// 操作审计失败直接拒绝执行（token 已消费，须重新 preview）；非高危尽力
	// 写、失败不阻断。sink == nil 时高危操作同样 fail-closed 拒绝：无审计
	// 通道的高危写绝不允许静默放行。
	auditSink := sink
	if !explicitSink {
		auditSink = a.Sink()
	}
	if auditSink == nil {
		if highRisk {
			dec.Denied = fmt.Errorf("%w: no audit sink configured", ErrAuditWrite)
			return dec, dec.Denied
		}
	} else {
		dec.Audited = true
		werr := auditSink.WriteAudit(ctx, AuditRecord{
			Actor:      "ai-bot",
			OnBehalfOf: userID,
			Tool:       tool,
			ParamsHash: hash,
			Result:     result,
			Action:     "picoclaw_write_confirm",
		})
		if werr != nil && highRisk {
			dec.Denied = fmt.Errorf("%w: %v", ErrAuditWrite, werr)
			return dec, dec.Denied
		}
	}

	dec.Allowed = true
	return dec, nil
}

// trimToken normalizes surrounding whitespace from token transport.
func trimToken(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
