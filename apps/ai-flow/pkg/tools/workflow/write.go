// [workflow-fork] P4-2: workflow 写工具的两阶段确认引擎（preview → confirm）
// 与真实写执行。
//
// 每个写调用都走同一条链路（P4 红线：写操作必须 explain preview 后等待人类
// 确认，无任何默认放行）：
//
//	confirm=false（或缺 token）→ 身份校验（workflow.RequireIdentity，P2-2）→
//	    高危动作角色门控（workflow.RequireRole）+ 高危单动作红线 →
//	    Approval.Issued() 签发 confirm_token（HMAC-SHA256，绑定
//	    user|tool|params_hash|expire）→ 返回 preview JSON（绝不触碰 API）。
//	confirm=true + token → 身份校验 → 角色门控/单动作红线 → Approval.Confirm()
//	    （P4-1：验签 / 过期 / 参数一致 / 单次使用；高危动作审计双写失败直接
//	    拒绝执行，fail-closed）→ 放行真实 workflow-api 写调用。
//
// 身份完全来自请求 context（identity 由鉴权网关注入，缺失即「未登录/缺
// JWT」错误）；模型无法通过 args 自报身份绕过：args 中的 user_id /
// on_behalf_of / roles 键在参数规范化时被剔除（不参与 hash，也不被信任）。
//
// 审计双写走 P4-1 的 HTTPAuditSink → workflow-api POST /audit-logs（actor
// 固定 ai-bot，on_behalf_of = JWT sub），因此所有确认执行在 audit_logs 可
// 按 on_behalf_of 查到。

package workflowtools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/computeflux/ai-flow/pkg/tools"
	"github.com/computeflux/ai-flow/pkg/workflow"
)

// WriteDeps is the per-loop wiring for the write tools: the shared typed
// client (base URL + tuning), the Approval broker (token signing /
// verification state; nil = zero-value fail-closed broker) and the
// AuditSink (audit_logs double-write target; nil = 高危确认被拒绝).
type WriteDeps struct {
	Client   *workflow.Client
	Approval *workflow.Approval
	Sink     workflow.AuditSink

	// BaseURLOverride, when set, makes every write/audit request target this
	// URL instead of the client's configured base (unit tests inject an
	// httptest server; production leaves it empty).
	BaseURLOverride string
}

// NewWriteDeps builds write deps for a typed client. The Approval broker is
// created from PICOCLAW_APPROVAL_SECRET (empty secret = secretless broker:
// preview still issues inert tokens, every confirm is rejected fail-closed)
// and the default HTTP audit sink against the same base URL.
func NewWriteDeps(c *workflow.Client, secret string) *WriteDeps {
	d := &WriteDeps{Client: c}
	if c != nil {
		d.Sink = workflow.NewHTTPAuditSink(c.BaseURL())
	}
	d.Approval = workflow.NewApprovalBroker(secret, 0, d.Sink, nil)
	return d
}

// SetClock overrides the approval broker clock (tests).
func (d *WriteDeps) SetClock(now func() time.Time) {
	if d.Approval != nil {
		d.Approval.SetClock(now)
	}
}

func (d *WriteDeps) approval() *workflow.Approval {
	if d.Approval == nil {
		return workflow.NewApprovalBroker("", 0, nil, nil)
	}
	return d.Approval
}

// writeClient returns the client used for real API calls, honoring the
// base-URL override.
func (d *WriteDeps) writeClient() *workflow.Client {
	c := d.Client
	if c == nil {
		return nil
	}
	if strings.TrimSpace(d.BaseURLOverride) == "" {
		return c
	}
	return c.WithBaseURLOverride(d.BaseURLOverride)
}

// auditSink returns the sink used for the confirm-phase audit double-write,
// honoring the base-URL override (the default HTTPAuditSink keeps its own
// base URL, so under an override a per-call adapter client is used).
func (d *WriteDeps) auditSink() workflow.AuditSink {
	if strings.TrimSpace(d.BaseURLOverride) == "" || d.Client == nil {
		return d.Sink
	}
	c := d.writeClient()
	var sink workflow.AuditSink = &httpAuditAdapter{client: c}
	return sink
}

// buildAuditSink constructs the confirm-phase audit sink from the deps
// configuration (base-URL override aware). Called once per confirm.
func (d *WriteDeps) buildAuditSink() workflow.AuditSink {
	return d.auditSink()
}

// defaultDeleteTarget is the delete target implied when args omit
// "target": the board tool deletes a board, the card tool deletes a card.
// Preview rendering and execution share this single resolution, so the
// human confirms exactly what will be executed.
func defaultDeleteTarget(toolName string) string {
	if toolName == ToolNameBoard {
		return "board"
	}
	return "card"
}

// httpAuditAdapter writes audit records through a typed client instance.
type httpAuditAdapter struct{ client *workflow.Client }

func (a *httpAuditAdapter) WriteAudit(ctx context.Context, rec workflow.AuditRecord) error {
	return a.client.Do(ctx, http.MethodPost, "/audit-logs", nil, rec, nil)
}

// confirmArgs extracts the confirm-phase transport parameters from args.
func confirmArgs(args map[string]any) (confirm bool, token string) {
	if args == nil {
		return false, ""
	}
	if c, ok := args["confirm"].(bool); ok {
		confirm = c
	}
	token, _ = argString(args, "confirm_token")
	return confirm, token
}

// canonicalWriteArgs strips the confirm-transport keys (confirm /
// confirm_token) and any identity-spoofing keys (user_id / on_behalf_of /
// roles / actions) from tool args for hashing + execution. Identity comes
// from the request context only; a model cannot bind a preview to another
// user or forge one. The multi-action "actions" list is stripped after the
// single-action gate has vetted it.
func canonicalWriteArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		switch k {
		case "confirm", "confirm_token", "user_id", "on_behalf_of", "roles", "actions":
			continue
		}
		out[k] = v
	}
	return out
}

// writeOutcome is the outcome of a fully passed confirm: Allowed=true and
// the executor has filled Result (typed decode target) / Raw body.
type writeOutcome struct {
	Allowed bool
	Result  any
	Raw     json.RawMessage
}

// runTwoPhase drives one write invocation through the P4-1 flow.
//
// Returns (previewResult, nil) for the preview phase, (nil, denialError) on
// any rejection (identity / role / single-action / token), and
// (nil, execError) when the confirm passed but the real API call failed.
// exec is called only after a fully successful confirm with the canonical
// write args.
func (d *WriteDeps) runTwoPhase(
	ctx context.Context,
	toolName string,
	args map[string]any,
	exec func(ctx context.Context, writeArgs map[string]any, out *writeOutcome) error,
) (*tools.ToolResult, *writeOutcome, error) {
	confirm, token := confirmArgs(args)
	writeArgs := canonicalWriteArgs(args)
	action, _ := argString(writeArgs, "action")

	// --- identity: every write requires a real login identity (P2-2). The
	// gateway handler injects both the Identity (user_id/roles) and the raw
	// JWT (workflow.ContextKeyJWT) into the tool-execution context; the
	// client forwards the JWT to workflow-api as Authorization: Bearer. ---
	id, err := workflow.RequireIdentity(ctx)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(workflow.JWTFromContext(ctx)) == "" {
		return nil, nil, fmt.Errorf("workflow api: no user token in request context (identity passthrough not established for this session); ask the user to retry from an authenticated board session")
	}

	// --- high-risk gates: single action + role (P4-2). Applied on BOTH
	// phases so preview issuance never smuggles a denied operation. ---
	if reason := multiActionReason(toolName, action, args); reason != "" {
		return nil, nil, fmt.Errorf("%s", reason)
	}
	if role, _ := RequiredRole(action); role != "" {
		if rerr := workflow.RequireRole(id, role); rerr != nil {
			return nil, nil, rerr
		}
	}

	// High-risk actions default their delete target per tool (board tool:
	// board; card tool: card) so the preview text and the executed
	// operation can never diverge.
	if action == ActionDelete {
		if _, ok := writeArgs["target"]; !ok {
			writeArgs["target"] = defaultDeleteTarget(toolName)
		}
	}

	approval := d.approval()

	if !confirm {
		// Preview phase: issue token + human-readable action; never touches
		// the real API. Missing secret → inert preview (P4-1 contract), the
		// confirm will fail closed.
		prev, perr := approval.Issued(id.UserID, toolName, PreviewAction(toolName, action, writeArgs), writeArgs)
		if perr != nil {
			return nil, nil, fmt.Errorf("approval preview: %w", perr)
		}
		res := jsonResult(prev)
		res.ForLLM = res.ForLLM +
			"\n\n[preview] 以上是本次写操作的预览。请把将要执行的动作向用户解释（explain），" +
			"等待人类确认后，携带 confirm=true 与 confirm_token 重新调用本工具执行。"
		return res, nil, nil
	}

	// confirm=true 但缺 token：100% 拒绝（P4-1 红线），绝不进入执行。
	if token == "" {
		return nil, nil, workflow.ErrNoToken
	}

	// --- confirm phase: verify (P4-1) → execute. The audit double-write
	// target is resolved per request and passed explicitly to the broker
	// (ConfirmWithSink): the shared broker's construction-time sink is never
	// mutated at runtime, so concurrent confirms can never observe or
	// deliver to another request's audit target. ---
	_, highRisk := RequiredRole(action)
	dec, cerr := approval.ConfirmWithSink(ctx, d.buildAuditSink(), id.UserID, toolName, writeArgs, token, highRisk, "confirmed_executed")
	if dec == nil {
		return nil, nil, cerr
	}
	if !dec.Allowed {
		denial := dec.Denied
		if denial == nil {
			denial = fmt.Errorf("approval denied: 写操作已被拒绝")
		}
		return nil, nil, denial
	}

	out := &writeOutcome{Allowed: true}
	if exec != nil {
		if err := exec(ctx, writeArgs, out); err != nil {
			return nil, nil, err
		}
	}
	return nil, out, nil
}
