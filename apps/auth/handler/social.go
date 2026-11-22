package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type socialRequest struct {
	Code         string `json:"code" binding:"required"`
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
	Device       string `json:"device"`
}

// Social POST /social/:provider（provider = google | github | x）
func (h *Handler) Social(c *gin.Context) {
	provider := c.Param("provider")
	var req socialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code required"})
		return
	}
	result, err := h.Login.Social(provider, req.Code, req.CodeVerifier, req.RedirectURI, req.Device, c.ClientIP())
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
