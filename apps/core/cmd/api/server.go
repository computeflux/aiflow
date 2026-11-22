package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/circuitbreaker"
	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/deploy"
	"github.com/wetee/ai-workflow/internal/dockerapi"
	"github.com/wetee/ai-workflow/internal/intervention"
	"github.com/wetee/ai-workflow/internal/notify"
	"github.com/wetee/ai-workflow/internal/skillgateway"
)

// registerSkillGatewayRoutes S1/S2/S3 网关装配：skill 调用网关（含熔断回调）+
// Agent /runs/* 权限收口 + S3 干预舱。逻辑自 main() 原样搬迁（批次 A 重构，行为无变更）。
func registerSkillGatewayRoutes(
	r *gin.Engine,
	deps *appDeps,
	brk *circuitbreaker.Registry,
	requireAuth func() gin.HandlerFunc,
) {
	database, apvStore := deps.database, deps.apvStore

	// S3 §3.1：breaker 首次 open 回调（写 intervention kind=circuit_open + 审计 + critical 告警）。
	onBreakerOpen := func(name string) {
		ctx := context.Background()
		_ = apvStore.CreateIntervention(ctx, name, "circuit_open",
			fmt.Sprintf("skill %s 连续失败 5 次，熔断器 open（60s 后半开探测）", name))
		apvStore.Log(ctx, "circuitbreaker", "circuit_open", name, map[string]any{"skill": name})
		deps.notifier.SendAlert(ctx, notify.Alert{
			Title: "skill 熔断器 open",
			Body:  fmt.Sprintf("skill %s 连续失败 5 次，已熔断（调用返回 503）", name),
			Level: "critical",
		})
		// S4 §3.3：skill 调用异常 → #agent-log
		deps.notifier.SendAgentLog(ctx, fmt.Sprintf("skill %s 连续失败 5 次，熔断器 open（调用返回 503，60s 后半开探测）", name))
	}
	skillGw := skillgateway.New(database).WithBreaker(brk, onBreakerOpen)
	if err := skillGw.EnsureSchema(); err != nil {
		log.Fatalf("ensure skill schema: %v", err)
	}
	skillgateway.RegisterRoutes(r.Group("/api/skills"), skillGw)
	// S2 §3.4：Agent 权限收口 —— /runs/*（只读查询 + Signal 白名单 approve/reject/inject + 取消）；
	// Agent 与 agent-prod workflow 交互的唯一 HTTP 入口（无直写权限），全程 audit_logs 留痕。
	approval.RegisterRunsRoutes(r, approval.NewRuns(deps.temporalAgent, apvStore), requireAuth())
	// S3 §3.3：干预舱 API（takeover/inject/retry/rollback/resolve + 活跃 workflow / 熔断状态查询）。
	intervention.RegisterRoutes(r, &intervention.Deps{
		Store:    intervention.NewStore(database),
		DefaultC: deps.temporal,
		AgentC:   deps.temporalAgent,
		Breaker:  brk,
		Approval: apvStore,
		Notifier: deps.notifier,
		Cfg:      deps.cfg,
	}, requireAuth())
	// 进程内 1 分钟 ticker：审批过期扫描 + 灰度状态机（重启后自动续推）
	go func() {
		ticker := time.NewTicker(time.Minute)
		for range ticker.C {
			if n, err := apvStore.TickExpiry(context.Background()); err != nil {
				log.Printf("WARN: approval expiry tick: %v", err)
			} else if n > 0 {
				log.Printf("approval: %d expired (no default pass; 升级通知已发)", n)
			}
			if _, err := apvStore.TickGray(context.Background()); err != nil {
				log.Printf("WARN: gray tick: %v", err)
			}
		}
	}()
	// S8 模型管理：1 分钟 ticker 拉取 model_profiles 活动档案，热切本进程 LLM
	// （端点/模型/嵌入模型；ApplyConfig 锁内换 inner client，进行中请求不受影响）。
	go func() {
		cur := ""
		apply := func() {
			p, err := apvStore.ActiveModelConfig()
			if err != nil || p == nil {
				return // 无 active 档案 → 保持 env 配置
			}
			sig := p.BaseURL + "|" + p.Model + "|" + p.EmbeddingModel
			if sig == cur {
				return
			}
			deps.llm.ApplyConfig(p.APIKey, p.BaseURL, p.EmbeddingModel, p.Model)
			cur = sig
			log.Printf("model-config: llm hot-switched to %s (%s @ %s)", p.Name, p.Model, p.BaseURL)
		}
		apply()
		ticker := time.NewTicker(time.Minute)
		for range ticker.C {
			apply()
		}
	}()
}

// newServiceOps 构造运维中心「运行服务总览」后端（S5）。Docker socket 与 REPO_ROOT
// 任一不可用则返回 error（调用方降级为未启用）。逻辑自 main() 原样搬迁（批次 A 重构，行为无变更）。
func newServiceOps(database *db.DB) (*deploy.ServiceOps, error) {
	sock := os.Getenv("DOCKER_HOST")
	if sock == "" {
		sock = "unix:///var/run/docker.sock"
	}
	if strings.HasPrefix(sock, "unix://") {
		if _, err := os.Stat(strings.TrimPrefix(sock, "unix://")); err != nil {
			return nil, fmt.Errorf("docker socket 不可用: %v", err)
		}
	}
	root := os.Getenv("REPO_ROOT")
	if root == "" {
		return nil, fmt.Errorf("REPO_ROOT 未设置（需指向统一 compose 仓库根）")
	}
	return deploy.NewServiceOps(database.DB, sock, root)
}

// newVersionManager 构造版本容器管理（S5 §3.4：灰度恶化回滚时自动 version down）。
// 逻辑自 main() 原样搬迁（批次 A 重构，行为无变更）。
func newVersionManager(database *db.DB) (*deploy.VersionManager, error) {
	sock := os.Getenv("DOCKER_HOST")
	if sock == "" {
		sock = "unix:///var/run/docker.sock"
	}
	if strings.HasPrefix(sock, "unix://") {
		if _, err := os.Stat(strings.TrimPrefix(sock, "unix://")); err != nil {
			return nil, fmt.Errorf("docker socket 不可用: %v", err)
		}
	}
	root := os.Getenv("REPO_ROOT")
	if root == "" {
		return nil, fmt.Errorf("REPO_ROOT 未设置（需指向统一 compose 仓库根）")
	}
	docker, err := dockerapi.New(sock)
	if err != nil {
		return nil, fmt.Errorf("docker client: %v", err)
	}
	project, err := deploy.LoadProject(root)
	if err != nil {
		return nil, fmt.Errorf("load compose project: %v", err)
	}
	return deploy.NewVersionManager(database.DB, docker, project, root), nil
}

// serveAPI 启动 HTTP 服务并等待退出信号后优雅关闭。
// 逻辑自 main() 原样搬迁（批次 A 重构，行为无变更）。
func serveAPI(cfg *config.Config, r *gin.Engine) {
	addr := fmt.Sprintf("%s:%s", cfg.AppHost, cfg.AppPort)
	srv := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Printf("API listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("run api: %v", err)
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Printf("shutting down API server")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("graceful shutdown: %v", err)
	}
}
