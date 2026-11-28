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
	"github.com/wetee/ai-workflow/internal/migration"
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
	requireRole func(roles ...string) gin.HandlerFunc,
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
	// M4-1（工作项 wi-1795489658-2d59）：跨服务切流 API（x-scan / like 的 addr 指向
	// social，10%→50%→100% 逐档放量）+ 旧服务缩容下线清单。
	//
	// ⚠️ 角色口径与其余「改生产流量」的写端点一致（服务重启/停止/回滚 = requireRole("admin")，
	// 见 main.go 的 serviceOps 路由组）：requireAuth() 只证明「是 board 被邀请的登录用户」，
	// 任何普通用户都能把 like/x-scan 的流量切到新实现或回滚版本 —— 切流的每一档都要人工确认，
	// 这个「人」必须是 admin。authVerifier 为空（未部署 apps/auth 的本地开发）时 requireRole
	// 是直通，不影响本地直连 :8001 调试。
	// 护栏阈值与稳定观察窗同一份 .env（CUTOVER_MIN_STAGE_SEC / CUTOVER_MIN_SAMPLES /
	// CUTOVER_PARITY_MIN / CUTOVER_SUCCESS_MIN / DECOMMISSION_STABLE_SEC，见 .env.example）：
	// 必须解析在构造 CutoverService **之前** —— 阈值是构造参数，写死成 CutoverConfig{Actor:
	// "cutover"} 就等于运维按 .env.example 把 CUTOVER_MIN_SAMPLES 调到 100、把 CUTOVER_PARITY_MIN
	// 提到 0.99 来收紧放量门槛时，API 仍按 20 / 0.9 放行 stage（「调紧了没生效」比「没这个开关」
	// 更危险）；而 cutoverSvc.Config() 又把这份写死的阈值喂给 skill-decommission/plan，两个端点
	// 一起偏离环境配置。
	// 解析失败不 fatal：LoadEnvConfig 只要任一字段类型不合法就返回 (nil, err)（它自己不会带回
	// 缺省值），而这份 EnvConfig 是 keepalive / 切流 / 下线三个组件共用的 —— 例如误写
	// CUTOVER_PARITY_MIN=0.9x，甚至与切流无关的 KEEPALIVE_STALE_SEC=3d。此时 envCfg == nil，
	// 下面两个缺省必须自己补齐：阈值走 WithDefaults，观察窗直接给 24h（= NewDecommission 对
	// <=0 的归一值，也是 DecommissionWindow 的缺省），保证「env 写错」只降级到缺省口径、
	// API 照常起来，而不是被后面的启动自检当成「切流护栏配置非法」直接起不来。
	envCfg, err := migration.LoadEnvConfig()
	if err != nil {
		log.Printf("migration env config: %v（用缺省阈值/窗口）", err)
	}
	cutoverCfg := migration.CutoverConfig{Actor: "cutover"}.WithDefaults()
	stableWindow := 24 * time.Hour
	if envCfg != nil {
		cutoverCfg = envCfg.CutoverConfig()
		stableWindow = envCfg.DecommissionWindow()
	}
	cutoverSvc := migration.NewCutover(database.DB, cutoverCfg,
		func(ctx context.Context, title, body, level string) error {
			return deps.notifier.SendAlert(ctx, notify.Alert{Title: title, Body: body, Level: level})
		})
	if err := cutoverSvc.EnsureSchema(); err != nil {
		log.Fatalf("ensure skill_cutover schema: %v", err)
	}
	migration.RegisterRoutes(r, cutoverSvc,
		migration.NewDecommission(database.DB, cutoverSvc.Config(), stableWindow), requireAuth(), requireRole("admin"))
	// MIG-1（工作项 wi-1795853890）步骤 2/5：注册表快照（apps/core/hacks/skill-registry/
	// mig1-social.yaml）的查看与同步端点。鉴权口径与切流完全一致：同步会往 skill_versions
	// 写版本行（改路由池），必须 admin；收尾前它只建零流量 draft，放量仍要走 /skill-cutover。
	// 快照根目录用 REPO_ROOT 覆盖（容器内挂载 monorepo 根的路径），缺省取当前工作目录
	// （cmd/api 本地跑时 = apps/core，向上可找到 deploy/docker-compose.yml）。
	registryRoot := os.Getenv("REPO_ROOT")
	if registryRoot == "" {
		if wd, werr := os.Getwd(); werr == nil {
			registryRoot = wd
		}
	}
	migration.RegistrySnapshotRoutes(r, registryRoot, database.DB, requireAuth(), requireRole("admin"))

	// 切流护栏阈值与下线观察窗都来自同一份 .env（装配期自检，让「配了没生效」在启动日志里
	// 现形）：NewCutover 的阈值是构造参数，一旦哪天改回写死 CutoverConfig{...}，.env 里的
	// CUTOVER_* 会静默失效（护栏照旧放行），而这份 cfg 又经 cutoverSvc.Config() 喂给
	// skill-decommission/plan —— 两个端点一起偏离环境配置，运行期没有任何信号。
	// 阈值全部 >0 由 WithDefaults / DecommissionWindow 保证；这里再兜一层非法值（含 env 把阈值
	// 配成负数、WithDefaults 的回落被绕过的情况），并把生效值打进启动日志：运维改完 .env 重启后
	// 对着这一行就能确认放量护栏真的按新值在跑。
	ctCfg := cutoverSvc.Config()
	if ctCfg.MinStageInterval <= 0 || ctCfg.MinSamples <= 0 || ctCfg.ParityMin <= 0 ||
		ctCfg.SuccessMin <= 0 || stableWindow <= 0 {
		log.Fatalf("切流护栏配置非法: %+v 观察窗=%s", ctCfg, stableWindow)
	}
	log.Printf("M4-1: 切流护栏生效（env）观察间隔=%s 每档最少样本=%d 一致率>=%g 成功率>=%g 下线观察窗=%s",
		ctCfg.MinStageInterval, ctCfg.MinSamples, ctCfg.ParityMin, ctCfg.SuccessMin, stableWindow)
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
