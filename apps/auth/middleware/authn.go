// Package middleware 提供 auth 服务自身的鉴权中间件
// （Bearer 本地验签 + 黑名单 + admin 判定）。
package middleware

import (
	"net/http"
	"strings"

	"github.com/computeflux/auth/config"
	"github.com/computeflux/auth/model"
	"github.com/computeflux/auth/service"
	"github.com/gin-gonic/gin"
)

const claimsKey = "auth_claims"

// Authn Bearer 验签（本地 RS256 + iss/aud/exp + 黑名单）。
type Authn struct {
	cfg    *config.Config
	signer *model.Signer
	sess   *service.Session
}

func NewAuthn(cfg *config.Config, signer *model.Signer, sess *service.Session) *Authn {
	return &Authn{cfg: cfg, signer: signer, sess: sess}
}

// RequireAuth 要求有效 Bearer token；通过后把 claims 放入 context。
func (a *Authn) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := a.signer.Verify(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		if claims.ID != "" && a.sess.IsBlacklisted(claims.ID) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token revoked"})
			return
		}
		c.Set(claimsKey, claims)
		c.Next()
	}
}

// RequireAdmin 要求管理员：X-Admin-Token 匹配，或 Bearer 含 admin 角色。
func (a *Authn) RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.cfg.AuthAdminToken != "" && c.GetHeader("X-Admin-Token") == a.cfg.AuthAdminToken {
			c.Next()
			return
		}
		token := bearerToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin credentials required"})
			return
		}
		claims, err := a.signer.Verify(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		if claims.ID != "" && a.sess.IsBlacklisted(claims.ID) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token revoked"})
			return
		}
		for _, r := range claims.Roles {
			if r == "admin" {
				c.Set(claimsKey, claims)
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin role required"})
	}
}

// Claims 从 context 取 claims（未鉴权时返回 nil）。
func Claims(c *gin.Context) *model.Claims {
	v, ok := c.Get(claimsKey)
	if !ok {
		return nil
	}
	claims, _ := v.(*model.Claims)
	return claims
}

// AdminBy 返回审计用的操作人（claims sub 或 "admin-token"）。
func AdminBy(c *gin.Context) string {
	if claims := Claims(c); claims != nil {
		return claims.Subject
	}
	return "admin-token"
}

func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
