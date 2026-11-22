// authn.go — 统一认证（apps/auth :8008）接入：JWKS 本地验签。
//
// 变更类接口（/api/deploy、/api/renew、私钥下载、配置读写、hosts 增删）
// 原来只认静态 MONITOR_DEPLOY_TOKEN；现在同时接受 auth 签发的 JWT
// （roles 含 admin 或 metrics:config:write 权限点）。静态 token 保留作兜底。
package main

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	authnOnce     sync.Once
	authnInstance *authnVerifier
)

type authnVerifier struct {
	authURL   string
	issuer    string
	audience  string
	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// authnEnabled 返回 auth 验签器（AUTH_URL 未配置时返回 nil = 仅静态 token）。
func authnEnabled() *authnVerifier {
	authnOnce.Do(func() {
		url := envOr("AUTH_URL", "")
		if url == "" {
			return
		}
		v := &authnVerifier{
			authURL:  strings.TrimRight(url, "/"),
			issuer:   envOr("AUTH_ISSUER", "https://auth.computeflux.ai"),
			audience: envOr("AUTH_AUDIENCE", "workflow"),
			keys:     map[string]*rsa.PublicKey{},
		}
		_ = v.refresh() // 启动时 auth 未就绪不阻断，请求路径按需重拉
		authnInstance = v
	})
	return authnInstance
}

func (v *authnVerifier) refresh() error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(v.authURL + "/jwks")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var doc struct {
		Keys []struct {
			KID string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.KID == "" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := new(big.Int).SetBytes(eBytes)
		v.keys[k.KID] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e.Int64())}
	}
	v.fetchedAt = time.Now()
	return nil
}

func (v *authnVerifier) publicKey(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	stale := time.Since(v.fetchedAt) > time.Hour
	key := v.keys[kid]
	v.mu.RUnlock()
	if key != nil {
		return key, nil
	}
	if stale {
		if err := v.refresh(); err != nil {
			return nil, err
		}
		v.mu.RLock()
		key = v.keys[kid]
		v.mu.RUnlock()
	}
	if key == nil {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return key, nil
}

// verifyJWT 验证 auth 签发的 access token，返回 (roles, ok)。
func (v *authnVerifier) verifyJWT(tokenStr string) ([]string, bool) {
	tok, err := jwt.ParseWithClaims(tokenStr, jwt.MapClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		kid, _ := t.Header["kid"].(string)
		return v.publicKey(kid)
	}, jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}))
	if err != nil || !tok.Valid {
		return nil, false
	}
	raw, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, false
	}
	if iss, _ := raw["iss"].(string); iss != v.issuer {
		return nil, false
	}
	audOK := false
	switch aud := raw["aud"].(type) {
	case string:
		audOK = aud == v.audience
	case []any:
		for _, a := range aud {
			if s, _ := a.(string); s == v.audience {
				audOK = true
			}
		}
	}
	if !audOK {
		return nil, false
	}
	exp, _ := raw["exp"].(float64)
	if exp == 0 || time.Now().Unix() >= int64(exp) {
		return nil, false
	}
	var roles []string
	if r, ok := raw["roles"].([]any); ok {
		for _, x := range r {
			if s, ok := x.(string); ok {
				roles = append(roles, s)
			}
		}
	}
	return roles, true
}

// authnAuthorized 检查 Bearer JWT 是否具备管理权限（admin 角色或 metrics:config:write）。
func authnAuthorized(r *http.Request) bool {
	v := authnEnabled()
	if v == nil {
		return false
	}
	h := r.Header.Get("Authorization")
	if len(h) <= 7 || !strings.EqualFold(h[:7], "bearer ") {
		return false
	}
	roles, ok := v.verifyJWT(strings.TrimSpace(h[7:]))
	if !ok {
		return false
	}
	for _, role := range roles {
		if role == "admin" {
			return true
		}
	}
	return false
}
