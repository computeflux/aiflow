// [workflow-fork] P2-2: ai-flow workflow 身份透传与角色收窄 helper
// (docs/plan/ai-bot-picoclaw-fork-plan.md §4，工作项 wi-1794237796-73c6)
//
// 身份链路（权限不放大）：
//
//	AIPanel --(Authorization: Bearer <user jwt>)--> gateway handler
//	        └─ 解析 JWT + user_id/roles → workflow.WithIdentity(ctx) 注入
//	             └─ pkg/tools/workflow/* 的 Execute(ctx) → FromContext(ctx) 消费
//	                  └─ workflow.Client 原样带 Bearer 调 workflow-api
//	                       └─ apps/auth JWKS 验签 + requireRole 判定  ← 唯一授权点
//
// 本文件不验签（验签只发生在 apps/auth，唯一授权点）；它只做两件事：
//  1. 在 context 中携带调用者身份（user_id / roles / 原始 JWT），供工具透传；
//  2. 提供 RequireRole 做工具侧「角色二次收窄」，越权返回带「所需角色」说明
//     的错误，避免模型自述绕过。
//
// 约定：identity 缺失（未登录 / 缺 JWT）时，所有 workflow 工具必须调用
// RequireIdentity / RequireRole 得到明确的「未登录/缺 JWT」错误，而不是 panic。

package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for identity checks. Callers may use errors.Is to branch;
// the default messages already carry the user-facing explanation.
var (
	// ErrNoIdentity: no Identity was injected into the context at all —
	// the request never went through an authenticating gateway handler.
	ErrNoIdentity = errors.New("workflow identity missing: 未登录/缺 JWT（请求未经过鉴权网关注入身份，请登录后重试）")
	// ErrNoJWT: identity exists in ctx but carries no JWT token.
	ErrNoJWT = errors.New("workflow identity missing JWT: 未登录/缺 JWT（缺少 Authorization Bearer token，请登录后重试）")
)

// Identity is the caller identity carried through the request context.
//
// It is injected by the gateway handler after parsing the incoming
// Authorization header and extracted by pkg/tools/workflow/* tool
// Execute(ctx) implementations — both to forward the raw JWT to the
// workflow API (single authorization point: apps/auth JWKS verify +
// requireRole) and to perform the tool-side role narrowing via
// RequireRole.
type Identity struct {
	// UserID is the authenticated user id (JWT subject) as resolved by the
	// gateway handler. Empty means "not resolved".
	UserID string
	// Roles are the roles granted to the user (e.g. "admin", "board_approver").
	Roles []string
	// JWT is the raw bearer token (without the "Bearer " scheme prefix).
	JWT string
}

// identityCtxKey is an unexported context key type — collision-free and only
// reachable through WithIdentity / IdentityFromContext below.
type identityCtxKey struct{}

// WithIdentity returns a child context carrying id. A nil identity is
// rejected: the caller keeps the original context (extraction then yields
// ErrNoIdentity, producing the explicit "未登录/缺 JWT" error downstream).
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	if ctx == nil || id == nil {
		return ctx
	}
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext extracts the Identity from ctx.
//
// Returns (nil, ErrNoIdentity) when none was injected. An identity without a
// JWT is reported with ErrNoJWT so tools can distinguish "no request
// identity" from "identity present, token missing".
func IdentityFromContext(ctx context.Context) (*Identity, error) {
	if ctx == nil {
		return nil, ErrNoIdentity
	}
	id, ok := ctx.Value(identityCtxKey{}).(*Identity)
	if !ok || id == nil {
		return nil, ErrNoIdentity
	}
	if strings.TrimSpace(id.JWT) == "" {
		return id, ErrNoJWT
	}
	return id, nil
}

// RequireIdentity returns the identity from ctx or a descriptive error
// (never panics). Use it in every workflow tool Execute that needs a user.
func RequireIdentity(ctx context.Context) (*Identity, error) {
	id, err := IdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id.UserID) == "" {
		return nil, errors.New("workflow identity missing user_id: 未登录/缺 JWT（身份缺少用户标识，请重新登录后重试）")
	}
	return id, nil
}

// RequireRole performs the tool-side role narrowing (二次收窄).
//
// It fails with:
//   - ErrNoIdentity / ErrNoJWT when the identity is missing (未登录/缺 JWT);
//   - a "需要 <role> 角色" error when the identity lacks the required role.
//
// The final authorization decision always belongs to apps/auth (JWKS verify +
// requireRole); this check only narrows what a tool will even attempt, so a
// model claiming elevated roles in its output cannot bypass it.
func RequireRole(id *Identity, role string) error {
	role = strings.TrimSpace(role)
	if id == nil {
		return ErrNoIdentity
	}
	if strings.TrimSpace(id.JWT) == "" {
		return ErrNoJWT
	}
	if role == "" {
		return errors.New("workflow require role: 内部错误（未指定所需角色）")
	}
	for _, r := range id.Roles {
		if strings.TrimSpace(r) == role {
			return nil
		}
	}
	return fmt.Errorf(
		"workflow permission denied: 越权操作被拒绝，该操作需要 %q 角色（当前用户 %s 角色：%s），请切换到具备该角色的账号后重试",
		role, id.UserID, strings.Join(roleList(id.Roles), " / "),
	)
}

// HasRole reports whether id carries the role (exact match after trimming).
func (id *Identity) HasRole(role string) bool {
	if id == nil {
		return false
	}
	role = strings.TrimSpace(role)
	for _, r := range id.Roles {
		if strings.TrimSpace(r) == role {
			return true
		}
	}
	return false
}

// roleList renders roles for error messages; empty → "无".
func roleList(roles []string) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return []string{"无"}
	}
	return out
}
