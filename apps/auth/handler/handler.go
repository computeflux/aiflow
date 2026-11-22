// Package handler HTTP 层（Gin）：只做参数校验与响应，业务逻辑在 service。
package handler

import (
	"errors"
	"net/http"

	"github.com/computeflux/auth/config"
	"github.com/computeflux/auth/model"
	"github.com/computeflux/auth/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Handler 聚合全部依赖。
type Handler struct {
	Cfg     *config.Config
	DB      *gorm.DB
	Signer  *model.Signer
	Login   *service.Login
	Wallet  *service.WalletLogin
	Otp     *service.Otp
	Session *service.Session
	Users   *service.UserSvc
	RBAC    *service.RBAC
	Audit   *service.Audit
}

// respondErr 统一错误响应 {"error": "..."} + 状态码。
func respondErr(c *gin.Context, err error) {
	var appErr *model.AppError
	if errors.As(err, &appErr) {
		c.JSON(appErr.Status, gin.H{"error": appErr.Msg})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
