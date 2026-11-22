package authn

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestVerifier 覆盖：JWKS 拉取/缓存、RS256 验签、iss/aud/exp 校验、kid 未知拒绝。
func TestVerifier(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	_ = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	// 本地 JWKS server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "test-kid", "use": "sig", "alg": "RS256",
			"n": base64url(key.PublicKey.N.Bytes()), "e": base64url(intBytes(key.PublicKey.E)),
		}}})
	}))
	defer srv.Close()

	v := NewVerifier(srv.URL, "https://issuer.test", "workflow")

	sign := func(over map[string]any) string {
		claims := map[string]any{
			"iss": "https://issuer.test", "sub": "user-1", "aud": "workflow",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"jti": "j1", "roles": []string{"admin"}, "email": "a@b.com",
		}
		for k, val := range over {
			claims[k] = val
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims))
		tok.Header["kid"] = "test-kid"
		s, _ := tok.SignedString(key)
		return s
	}

	// 正常验签
	c, err := v.Verify(sign(nil))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.UserID != "user-1" || c.JTI != "j1" || len(c.Roles) != 1 || c.Roles[0] != "admin" || c.Email != "a@b.com" {
		t.Fatalf("claims mismatch: %+v", c)
	}

	// 错误 issuer
	if _, err := v.Verify(sign(map[string]any{"iss": "https://other"})); err == nil {
		t.Fatal("expected issuer mismatch")
	}
	// 错误 audience
	if _, err := v.Verify(sign(map[string]any{"aud": "other"})); err == nil {
		t.Fatal("expected audience mismatch")
	}
	// 过期
	if _, err := v.Verify(sign(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})); err == nil {
		t.Fatal("expected expired")
	}
	// 未知 kid
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://issuer.test", "sub": "u", "aud": "workflow", "exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "unknown"
	s, _ := tok.SignedString(key)
	if _, err := v.Verify(s); err == nil {
		t.Fatal("expected unknown kid rejection")
	}
}

func base64url(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		out = append(out, alphabet[chunk[0]>>2])
		out = append(out, alphabet[((chunk[0]&0x3)<<4)|(chunk[1]>>4)&0xF])
		if n > 1 {
			out = append(out, alphabet[(chunk[1]<<2)&0x3F|(chunk[2]>>6)&0x3F])
		}
		if n > 2 {
			out = append(out, alphabet[chunk[2]&0x3F])
		}
	}
	return string(out)
}

func intBytes(v int) []byte {
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	// 去前导零
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}
