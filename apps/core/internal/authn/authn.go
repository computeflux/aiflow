// Package authn 提供 apps/auth（:8008）JWT 的本地验签中间件。
//
// 下游不逐请求调 auth：拉 GET /jwks 缓存公钥（按 kid 索引，TTL 1h）→
// 本地验 RS256 签名 + iss/aud/exp → 取 sub/roles。
// 粗粒度（页面级）用 JWT 里的 roles 快照；细粒度即时生效走 auth 的 /authorize。
package authn

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	jwksTTL      = time.Hour
	claimsCtxKey = "authn_claims"
	userIDCtxKey = "authn_user_id"
	rolesCtxKey  = "authn_roles"
)

// Claims 下游关心的 claims 子集。
type Claims struct {
	UserID string
	Roles  []string
	Email  string
	JTI    string
	Exp    time.Time
}

// Verifier 本地验签器（JWKS 缓存 + RS256）。
type Verifier struct {
	AuthURL  string // 如 http://127.0.0.1:8008
	Issuer   string
	Audience string

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey // kid → 公钥
	fetchedAt time.Time
}

// NewVerifier 创建验签器并预拉 JWKS（失败不阻断启动，首次请求时重试）。
func NewVerifier(authURL, issuer, audience string) *Verifier {
	v := &Verifier{
		AuthURL:  strings.TrimRight(authURL, "/"),
		Issuer:   issuer,
		Audience: audience,
		keys:     map[string]*rsa.PublicKey{},
	}
	if err := v.refresh(); err != nil {
		// 启动时 auth 可能未就绪；记录后继续，请求路径会按需重拉
		_ = err
	}
	return v
}

// refresh 拉取 JWKS 并合并公钥（轮换期双 kid 并存，旧 kid 保留到 TTL 过期）。
func (v *Verifier) refresh() error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(v.AuthURL + "/jwks")
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
		return fmt.Errorf("jwks decode: %w", err)
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

func (v *Verifier) publicKey(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	stale := time.Since(v.fetchedAt) > jwksTTL
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

// decodeSegment 解码 base64url JSON 段（JWT header / claims 的编码约定：
// 无填充 + 标准 padding 两种写法都接受，同 golang-jwt 的默认 decoding）。
func decodeSegment(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// Verify 手动解析 + 验签（RS256）：header.payload.signature 三段 base64url，
// 签名 = RSASSA-PKCS1-v1_5(SHA-256) over "header.payload" 原始字节。
// 标准 claims 校验：iss 精确匹配、aud 字符串/数组含目标值、exp 未过期；
// 算法锁定 RS256（其它 alg 直接拒绝，防算法混淆）。
func (v *Verifier) Verify(tokenStr string) (*Claims, error) {
	parts := strings.Split(strings.TrimSpace(tokenStr), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token: want 3 segments, got %d", len(parts))
	}
	headerRaw, err := decodeSegment(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unexpected signing method: %v", header.Alg)
	}
	sig, err := decodeSegment(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	pub, err := v.publicKey(header.KID)
	if err != nil {
		return nil, err
	}
	signed := parts[0] + "." + parts[1]
	hash := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig); err != nil {
		return nil, errors.New("signature invalid")
	}
	claimsRaw, err := decodeSegment(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(claimsRaw, &raw); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	// 标准 claims 手动校验（iss/aud/exp）
	if iss, _ := raw["iss"].(string); iss != v.Issuer {
		return nil, fmt.Errorf("issuer mismatch")
	}
	audOK := false
	switch aud := raw["aud"].(type) {
	case string:
		audOK = aud == v.Audience
	case []any:
		for _, a := range aud {
			if s, _ := a.(string); s == v.Audience {
				audOK = true
			}
		}
	}
	if !audOK {
		return nil, fmt.Errorf("audience mismatch")
	}
	exp, _ := raw["exp"].(float64)
	if exp == 0 || time.Now().Unix() >= int64(exp) {
		return nil, fmt.Errorf("token expired")
	}
	sub, _ := raw["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("missing sub")
	}
	var roles []string
	if r, ok := raw["roles"].([]any); ok {
		for _, x := range r {
			if s, ok := x.(string); ok {
				roles = append(roles, s)
			}
		}
	}
	jti, _ := raw["jti"].(string)
	email, _ := raw["email"].(string)
	return &Claims{
		UserID: sub,
		Roles:  roles,
		Email:  email,
		JTI:    jti,
		Exp:    time.Unix(int64(exp), 0),
	}, nil
}

// ---- 中间件 ----

// RequireAuth 要求有效 Bearer token；通过后把 user_id/roles 放入 context。
func (v *Verifier) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := v.Verify(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		c.Set(claimsCtxKey, claims)
		c.Set(userIDCtxKey, claims.UserID)
		c.Set(rolesCtxKey, claims.Roles)
		c.Next()
	}
}

// RequireRole 要求登录且拥有任一指定角色（JWT roles 快照，≤15min 生效）。
func (v *Verifier) RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := v.Verify(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		for _, want := range roles {
			for _, has := range claims.Roles {
				if has == want {
					c.Set(claimsCtxKey, claims)
					c.Set(userIDCtxKey, claims.UserID)
					c.Set(rolesCtxKey, claims.Roles)
					c.Next()
					return
				}
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient role"})
	}
}

// OptionalAuth 有有效 token 则解析入 context，无/无效不拦截（用于审计补操作人）。
func (v *Verifier) OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token != "" {
			if claims, err := v.Verify(token); err == nil {
				c.Set(claimsCtxKey, claims)
				c.Set(userIDCtxKey, claims.UserID)
				c.Set(rolesCtxKey, claims.Roles)
			}
		}
		c.Next()
	}
}

// UserID 从 context 取操作人（未鉴权时为空）。
func UserID(c *gin.Context) string {
	v, _ := c.Get(userIDCtxKey)
	s, _ := v.(string)
	return s
}

// Roles 从 context 取角色（未鉴权时为空）。
func Roles(c *gin.Context) []string {
	v, _ := c.Get(rolesCtxKey)
	roles, _ := v.([]string)
	return roles
}

func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
