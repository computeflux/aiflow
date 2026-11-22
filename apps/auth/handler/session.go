package handler

import (
	"net/http"

	"github.com/computeflux/auth/middleware"
	"github.com/gin-gonic/gin"
)

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
	Device       string `json:"device"`
}

// Refresh POST /refresh（refresh 轮换）
func (h *Handler) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refresh_token required"})
		return
	}
	result, err := h.Session.Refresh(req.RefreshToken, req.Device, c.ClientIP())
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Logout POST /logout（需 Bearer；带 refresh_token 只吊销该会话，否则全部）
func (h *Handler) Logout(c *gin.Context) {
	var req logoutRequest
	_ = c.ShouldBindJSON(&req) // 允许空 body
	if err := h.Session.Logout(middleware.Claims(c), req.RefreshToken); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Me GET /me（需 Bearer）
func (h *Handler) Me(c *gin.Context) {
	claims := middleware.Claims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return
	}
	user, roles, err := h.userDetail(claims.Subject)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":          user.ID,
		"email":       user.Email,
		"name":        user.Name,
		"avatar":      user.Avatar,
		"wallet_addr": user.WalletAddr,
		"roles":       roles,
		"status":      statusStr(user.Status),
	})
}
