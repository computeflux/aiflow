package handler

import (
	"net/http"

	"github.com/computeflux/auth/model"
	"github.com/gin-gonic/gin"
)

// Healthz GET /healthz（含 DB ping）
func (h *Handler) Healthz(c *gin.Context) {
	sqlDB, err := h.DB.DB()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if pingErr := sqlDB.PingContext(c.Request.Context()); pingErr != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "db ping: " + pingErr.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "service": "auth"})
}

// JWKS GET /jwks（公钥集，kid 索引，支持轮换；无需鉴权）
func (h *Handler) JWKS(c *gin.Context) {
	c.JSON(http.StatusOK, model.JWKS([]*model.KeyPair{h.Signer.KeyPair}))
}

// Blacklist GET /blacklist/{jti}（200 {"revoked":true} / 404 未拉黑；仅关键操作调用）
func (h *Handler) Blacklist(c *gin.Context) {
	if h.Session.IsBlacklisted(c.Param("jti")) {
		c.JSON(http.StatusOK, gin.H{"revoked": true})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"revoked": false})
}
