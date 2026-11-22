// auth — 统一权限认证服务（自带 IdP：邮箱 OTP + 社交登录 + JWT + RBAC）。
// 设计见 README.md + docs/。
package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/computeflux/auth/config"
	"github.com/computeflux/auth/handler"
	"github.com/computeflux/auth/middleware"
	"github.com/computeflux/auth/model"
	"github.com/computeflux/auth/service"
	"github.com/computeflux/auth/store"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

//go:embed web/index.html
var uiHTML []byte

func main() {
	cfg := config.Load()
	db, err := store.Open(cfg)
	if err != nil {
		log.Fatalf("[auth] open db: %v", err)
	}

	// RS256 密钥对：文件不存在时首次启动自动生成（0600，不落库）。
	keyPair, created, err := model.LoadOrCreateKeyPair(cfg.AuthJWTPrivateKeyPath, cfg.AuthJWTKid)
	if err != nil {
		log.Fatalf("[auth] jwt key: %v", err)
	}
	if created {
		log.Printf("[auth] 已生成新的 RS256 密钥对（%s，kid=%s）", cfg.AuthJWTPrivateKeyPath, keyPair.Kid)
	}
	signer := &model.Signer{
		KeyPair:   keyPair,
		Issuer:    cfg.AuthIssuer,
		Audience:  cfg.AuthAudience,
		AccessTTL: time.Duration(cfg.AuthAccessTTLSec) * time.Second,
	}

	audit := service.NewAudit(db)
	mailer := service.NewMailer(cfg)
	if !mailer.Enabled() {
		log.Printf("[auth] WARN: SMTP 未配置，邮箱 OTP 不可用（社交登录不受影响）")
	}
	otp := service.NewOtp(db, mailer, audit)
	oauth := service.NewOAuth(cfg)
	users := service.NewUserSvc(db, cfg, audit)
	session := service.NewSession(db, cfg, signer, audit)
	rbac := service.NewRBAC(db, audit)
	login := service.NewLogin(otp, oauth, users, session)
	wallet := service.NewWalletLogin(users, session, cfg)
	authn := middleware.NewAuthn(cfg, signer, session)

	h := &handler.Handler{
		Cfg: cfg, DB: db, Signer: signer,
		Login: login, Wallet: wallet, Otp: otp, Session: session, Users: users, RBAC: rbac, Audit: audit,
	}

	r := gin.New()
	r.SetTrustedProxies(nil) // 不信任代理头（ClientIP 防伪造）
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.CORSOrigins(),
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Admin-Token"},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}))

	// 管理前端（单文件 SPA，go:embed；登录走 OTP/admin token）
	r.GET("/ui", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", uiHTML)
	})

	// 运维 / 公钥
	r.GET("/healthz", h.Healthz)
	r.GET("/jwks", h.JWKS)
	r.GET("/blacklist/:jti", h.Blacklist)

	// 登录（面向前端 web3auth SDK，CORS 开放）
	r.POST("/otp/request", h.RequestOtp)
	r.POST("/otp/verify", h.VerifyOtp)
	r.POST("/social/:provider", h.Social)
	r.POST("/refresh", h.Refresh)
	r.POST("/gql", h.GQL) // web3auth SDK 兼容层（GraphQL mutations）

	// 会话（需 Bearer）
	g := r.Group("/", authn.RequireAuth())
	g.POST("/logout", h.Logout)
	g.GET("/me", h.Me)

	// 权限判定（面向业务服务：Bearer 透传或 X-Admin-Token）
	a := r.Group("/", authn.RequireAdmin())
	a.POST("/authorize", h.Authorize)
	a.GET("/users/:id/permissions", h.UserPermissions)

	// 用户管理（需管理员）
	a.GET("/users", h.ListUsers)
	a.POST("/users", h.CreateUser)
	a.GET("/users/:id", h.GetUser)
	a.POST("/users/:id/disable", h.DisableUser)
	a.POST("/users/:id/enable", h.EnableUser)
	a.DELETE("/users/:id", h.DeleteUser)
	a.GET("/users/:id/roles", h.ListUserRoles)
	a.POST("/users/:id/roles", h.GrantRole)
	a.DELETE("/users/:id/roles/:role", h.RevokeRole)
	a.POST("/users/:id/permissions", h.GrantPermission)
	a.DELETE("/users/:id/permissions/:permission", h.RevokePermission)

	// 角色与权限点（需管理员）
	a.GET("/roles", h.ListRoles)
	a.POST("/roles", h.CreateRole)
	a.PUT("/roles/:id", h.UpdateRole)
	a.DELETE("/roles/:id", h.DeleteRole)
	a.GET("/permissions", h.ListPermissions)
	a.POST("/permissions", h.RegisterPermission)

	// 审计（需管理员）
	a.GET("/audit", h.ListAudit)

	// 后台清理：过期黑名单 / refresh / OTP（10min）
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			session.Cleanup()
		}
	}()

	addr := cfg.AuthHost + ":" + cfg.AuthPort
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("[auth] listening on %s (issuer=%s aud=%s kid=%s)", addr, cfg.AuthIssuer, cfg.AuthAudience, keyPair.Kid)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[auth] run: %v", err)
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Printf("[auth] shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("[auth] graceful shutdown: %v", err)
	}
}
