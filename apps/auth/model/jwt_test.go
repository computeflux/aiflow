package model

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSignVerifyJWKS 覆盖：密钥生成/加载、签发、验签、JWKS 导出、黑名单 claims。
func TestSignVerifyJWKS(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "test.key")

	kp, created, err := LoadOrCreateKeyPair(keyPath, "")
	if err != nil {
		t.Fatalf("LoadOrCreateKeyPair: %v", err)
	}
	if !created {
		t.Fatal("expected key creation on first run")
	}
	if st, serr := os.Stat(keyPath); serr != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file perms: %v %v", st, serr)
	}

	// 二次加载复用同一密钥
	kp2, created2, err := LoadOrCreateKeyPair(keyPath, "")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if created2 {
		t.Fatal("expected reuse on second run")
	}
	if kp2.Kid != kp.Kid {
		t.Fatalf("kid mismatch: %s vs %s", kp2.Kid, kp.Kid)
	}

	signer := &Signer{KeyPair: kp, Issuer: "https://test", Audience: "workflow", AccessTTL: 15 * time.Minute}
	tok, jti, exp, err := signer.Issue("user-1", []string{"admin", "user"}, "a@b.com", "0xabc")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if jti == "" || exp.Before(time.Now().Add(14*time.Minute)) {
		t.Fatalf("bad jti/exp: %s %v", jti, exp)
	}

	claims, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "user-1" || claims.ID != jti || len(claims.Roles) != 2 || claims.Email != "a@b.com" {
		t.Fatalf("claims mismatch: %+v", claims)
	}

	// 错误 issuer 应被拒绝
	badSigner := &Signer{KeyPair: kp, Issuer: "https://other", Audience: "workflow", AccessTTL: time.Minute}
	if _, err := badSigner.Verify(tok); err == nil {
		t.Fatal("expected issuer mismatch to fail")
	}

	// JWKS 导出
	jwks := JWKS([]*KeyPair{kp})
	keys, ok := jwks["keys"].([]map[string]any)
	if !ok || len(keys) != 1 || keys[0]["kid"] != kp.Kid || keys[0]["kty"] != "RSA" {
		t.Fatalf("jwks mismatch: %v", jwks)
	}
}
