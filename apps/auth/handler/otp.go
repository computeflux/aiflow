package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type otpRequest struct {
	Email string `json:"email" binding:"required"`
}

type otpVerifyRequest struct {
	Email  string `json:"email" binding:"required"`
	Code   string `json:"code" binding:"required"`
	Device string `json:"device"`
}

// RequestOtp POST /otp/request
func (h *Handler) RequestOtp(c *gin.Context) {
	var req otpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email required"})
		return
	}
	if err := h.Otp.Request(req.Email, c.ClientIP()); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// VerifyOtp POST /otp/verify
func (h *Handler) VerifyOtp(c *gin.Context) {
	var req otpVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email and code required"})
		return
	}
	result, err := h.Login.VerifyEmail(req.Email, req.Code, req.Device, c.ClientIP())
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
