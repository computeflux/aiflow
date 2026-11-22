// [workflow-fork] P2-2 unit tests for identity passthrough & role narrowing.

package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWithIdentityAndFromContext(t *testing.T) {
	id := &Identity{UserID: "u-1", Roles: []string{"user", "board_approver"}, JWT: "jwt-token"}
	ctx := WithIdentity(context.Background(), id)

	got, err := IdentityFromContext(ctx)
	if err != nil {
		t.Fatalf("IdentityFromContext: %v", err)
	}
	if got != id {
		t.Fatalf("IdentityFromContext returned %v, want the injected identity", got)
	}
	if got.UserID != "u-1" || got.JWT != "jwt-token" || len(got.Roles) != 2 {
		t.Fatalf("identity fields mismatch: %+v", got)
	}
}

func TestFromContextMissing(t *testing.T) {
	// No identity injected → ErrNoIdentity (the explicit 未登录/缺 JWT path).
	got, err := IdentityFromContext(context.Background())
	if got != nil {
		t.Fatalf("expected nil identity, got %+v", got)
	}
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("expected ErrNoIdentity, got %v", err)
	}
	if !strings.Contains(err.Error(), "未登录/缺 JWT") {
		t.Fatalf("ErrNoIdentity message must explain 未登录/缺 JWT: %q", err)
	}

	// nil ctx must not panic.
	if _, err := IdentityFromContext(nil); !errors.Is(err, ErrNoIdentity) { //nolint:staticcheck
		t.Fatalf("nil ctx: expected ErrNoIdentity, got %v", err)
	}
}

func TestWithIdentityNilIsNoop(t *testing.T) {
	ctx := WithIdentity(context.Background(), nil)
	if _, err := IdentityFromContext(ctx); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("nil identity injection must yield ErrNoIdentity, got %v", err)
	}
}

func TestIdentityFromContextMissingJWT(t *testing.T) {
	ctx := WithIdentity(context.Background(), &Identity{UserID: "u-1", Roles: []string{"admin"}})
	got, err := IdentityFromContext(ctx)
	if got == nil {
		t.Fatal("expected identity to be returned alongside ErrNoJWT")
	}
	if !errors.Is(err, ErrNoJWT) {
		t.Fatalf("expected ErrNoJWT, got %v", err)
	}
	if !strings.Contains(err.Error(), "未登录/缺 JWT") {
		t.Fatalf("ErrNoJWT message must explain 未登录/缺 JWT: %q", err)
	}
}

func TestRequireIdentity(t *testing.T) {
	ctx := WithIdentity(context.Background(), &Identity{UserID: "u-1", JWT: "jwt"})
	id, err := RequireIdentity(ctx)
	if err != nil || id.UserID != "u-1" {
		t.Fatalf("RequireIdentity: %v %+v", err, id)
	}

	// Missing identity → explicit error, no panic.
	if _, err := RequireIdentity(context.Background()); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("expected ErrNoIdentity, got %v", err)
	}

	// Identity without user_id → explicit error mentioning 未登录/缺 JWT.
	ctx = WithIdentity(context.Background(), &Identity{JWT: "jwt"})
	if _, err := RequireIdentity(ctx); err == nil || !strings.Contains(err.Error(), "未登录/缺 JWT") {
		t.Fatalf("expected missing user_id error, got %v", err)
	}
}

func TestRequireRoleGranted(t *testing.T) {
	id := &Identity{UserID: "u-1", Roles: []string{"user", " admin ", "board_approver"}, JWT: "jwt"}
	if err := RequireRole(id, "admin"); err != nil {
		t.Fatalf("RequireRole(admin): %v", err)
	}
	if err := RequireRole(id, "board_approver"); err != nil {
		t.Fatalf("RequireRole(board_approver): %v", err)
	}
}

func TestRequireRoleDenied(t *testing.T) {
	id := &Identity{UserID: "u-1", Roles: []string{"user"}, JWT: "jwt"}
	err := RequireRole(id, "board_approver")
	if err == nil {
		t.Fatal("expected role denial")
	}
	msg := err.Error()
	for _, want := range []string{"越权", "board_approver"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("denial message must mention %q: %q", want, msg)
		}
	}
}

func TestRequireRoleNilIdentity(t *testing.T) {
	// nil identity → the explicit 未登录/缺 JWT sentinel, never a panic.
	err := RequireRole(nil, "admin")
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("expected ErrNoIdentity, got %v", err)
	}
	if !strings.Contains(err.Error(), "未登录/缺 JWT") {
		t.Fatalf("message must explain 未登录/缺 JWT: %q", err)
	}
}

func TestRequireRoleMissingJWT(t *testing.T) {
	id := &Identity{UserID: "u-1", Roles: []string{"admin"}}
	err := RequireRole(id, "admin")
	if !errors.Is(err, ErrNoJWT) {
		t.Fatalf("expected ErrNoJWT, got %v", err)
	}
}

func TestHasRole(t *testing.T) {
	id := &Identity{Roles: []string{"user", "admin"}}
	if !id.HasRole("admin") || id.HasRole("board_approver") {
		t.Fatal("HasRole mismatch")
	}
	var nilID *Identity
	if nilID.HasRole("admin") {
		t.Fatal("nil identity must not have any role")
	}
}

func TestRoleList(t *testing.T) {
	if got := strings.Join(roleList(nil), ","); got != "无" {
		t.Fatalf("empty roles should render 无, got %q", got)
	}
	if got := strings.Join(roleList([]string{" a ", "", "b"}), ","); got != "a,b" {
		t.Fatalf("roleList trim/filter failed: %q", got)
	}
}
