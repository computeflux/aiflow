package handler

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/computeflux/auth/model"
	"github.com/gin-gonic/gin"
)

// gql 兼容层：web3auth 前端 SDK 的登录走 GraphQL（POST /gql，
// mutations: requestOTP / verifyOTP / socialLogin）。本服务核心是 REST，
// 这里把 SDK 的固定 mutation 映射到同一套 service，使 SDK 无需改动即可接入
// （backendUrl 指向本服务即可）。

var gqlOpRe = regexp.MustCompile(`(?s)^\s*(?:mutation|query)\s*(?:\([^)]*\))?\s*\{\s*(\w+)`)

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// GQL POST /gql（web3auth SDK 兼容；公开端点，受全局限流保护）
func (h *Handler) GQL(c *gin.Context) {
	var req gqlRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"message": "bad request"}}})
		return
	}
	m := gqlOpRe.FindStringSubmatch(req.Query)
	if m == nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"message": "unsupported query"}}})
		return
	}
	switch m[1] {
	case "requestOTP":
		h.gqlRequestOTP(c, req.Variables)
	case "verifyOTP":
		h.gqlVerifyOTP(c, req.Variables)
	case "socialLogin":
		h.gqlSocialLogin(c, req.Variables)
	case "requestWalletNonce":
		h.gqlRequestWalletNonce(c, req.Variables)
	case "loginWithWallet":
		h.gqlLoginWithWallet(c, req.Variables)
	case "checkSession":
		h.gqlCheckSession(c, req.Variables)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"message": "unknown mutation " + m[1]}}})
	}
}

func gqlVar(vars map[string]any, key string) string {
	if v, ok := vars[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func gqlOK(c *gin.Context, field string, payload gin.H) {
	c.JSON(http.StatusOK, gin.H{"data": gin.H{field: payload}})
}

func gqlErr(c *gin.Context, field, msg string) {
	c.JSON(http.StatusOK, gin.H{"data": gin.H{field: gin.H{"success": false, "error": msg}}})
}

func (h *Handler) gqlRequestOTP(c *gin.Context, vars map[string]any) {
	email := gqlVar(vars, "email")
	if err := h.Otp.Request(email, c.ClientIP()); err != nil {
		gqlErr(c, "requestOTP", err.Error())
		return
	}
	gqlOK(c, "requestOTP", gin.H{"success": true})
}

func (h *Handler) gqlVerifyOTP(c *gin.Context, vars map[string]any) {
	email := gqlVar(vars, "email")
	code := gqlVar(vars, "otp")
	result, err := h.Login.VerifyEmail(email, code, "", c.ClientIP())
	if err != nil {
		gqlErr(c, "verifyOTP", err.Error())
		return
	}
	gqlOK(c, "verifyOTP", gqlLoginPayload(result, "email_passwordless"))

}

func (h *Handler) gqlSocialLogin(c *gin.Context, vars map[string]any) {
	provider := strings.ToLower(gqlVar(vars, "provider"))
	if provider == "twitter" {
		provider = "x"
	}
	result, err := h.Login.Social(provider, gqlVar(vars, "code"), gqlVar(vars, "codeVerifier"),
		gqlVar(vars, "redirectUri"), "", c.ClientIP())
	if err != nil {
		gqlErr(c, "socialLogin", err.Error())
		return
	}
	gqlOK(c, "socialLogin", gqlLoginPayload(result, provider))
}

// gqlCheckSession 会话健康检查：本地验签 token（web3auth SDK 登录后定期调用）。
func (h *Handler) gqlCheckSession(c *gin.Context, vars map[string]any) {
	token := gqlVar(vars, "token")
	if token == "" {
		gqlErr(c, "checkSession", "missing token")
		return
	}
	if _, err := h.Signer.Verify(token); err != nil {
		gqlErr(c, "checkSession", "invalid session")
		return
	}
	gqlOK(c, "checkSession", gin.H{"success": true})
}

// gqlRequestWalletNonce 钱包登录第一步：为地址生成一次性 nonce。
func (h *Handler) gqlRequestWalletNonce(c *gin.Context, vars map[string]any) {
	address := gqlVar(vars, "address")
	nonce, err := h.Wallet.RequestNonce(address)
	if err != nil {
		gqlErr(c, "requestWalletNonce", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"requestWalletNonce": nonce}})
}

// gqlLoginWithWallet 钱包登录第二步：验证 EIP-191 签名 → 签发 token 对。
func (h *Handler) gqlLoginWithWallet(c *gin.Context, vars map[string]any) {
	result, err := h.Wallet.LoginWithWallet(
		gqlVar(vars, "address"), gqlVar(vars, "signature"),
		gqlVar(vars, "message"), gqlVar(vars, "nonce"), c.ClientIP())
	if err != nil {
		gqlErr(c, "loginWithWallet", err.Error())
		return
	}
	gqlOK(c, "loginWithWallet", gqlLoginPayload(result, "wallet"))
}

// gqlLoginPayload 把 REST 登录响应映射为 SDK 期望的字段
// （token/refreshToken/address/userInfo{email name profileImage typeOfLogin}）。
// address：有钱包地址用钱包地址，否则回退 user_id（SDK 要求非空才能持久化会话）。
func gqlLoginPayload(result *model.LoginResult, typeOfLogin string) gin.H {
	uid, email, name := "", "", ""
	if result.User != nil {
		uid, email, name = result.User.ID, result.User.Email, result.User.Name
	}
	wallet := ""
	if wallet == "" {
		wallet = uid
	}
	return gin.H{
		"success":      true,
		"token":        result.AccessToken,
		"refreshToken": result.RefreshToken,
		"address":      wallet,
		"userInfo": gin.H{
			"email":        email,
			"name":         name,
			"profileImage": "",
			"typeOfLogin":  typeOfLogin,
		},
	}
}
