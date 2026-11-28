// Package main is the api binary.
//
// api 是工作流核心 REST API（Gin :8001；board/内容/运维/产品四域路由 + ticker）。
// main() 只负责依赖装配：构造 → appDeps → 各域路由注册（routes_*.go / server.go）→ 启动服务。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/circuitbreaker"
	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/crag"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/deploy"
	"github.com/wetee/ai-workflow/internal/docstore"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/notify"
	"github.com/wetee/ai-workflow/internal/ops"
	"github.com/wetee/ai-workflow/internal/otel"
	internalotel "github.com/wetee/ai-workflow/internal/otel"
	"github.com/wetee/ai-workflow/internal/product"
	"github.com/wetee/ai-workflow/internal/project"
	"github.com/wetee/ai-workflow/internal/qdrant"
	"github.com/wetee/ai-workflow/internal/workitem"
	"go.temporal.io/sdk/client"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
	"gorm.io/plugin/opentelemetry/tracing"
)

func main() {
	cfg := config.Load()

	database, err := db.Open(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}

	// OTel 初始化：上报到 otel-collector（默认 http://127.0.0.1:4317）
	otelShutdown, err := internalotel.Init(context.Background(), "workflow-api")
	if err != nil {
		log.Fatalf("init otel: %v", err)
	}
	defer otelShutdown(context.Background())

	// gorm 自动埋 SQL span（失败不阻断启动）
	if err := database.Use(tracing.NewPlugin(tracing.WithoutMetrics())); err != nil {
		log.Printf("enable gorm tracing plugin: %v", err)
	}
	llmClient := llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.EmbeddingModel, cfg.LLMModel)
	qc, err := qdrant.NewClient(cfg.QdrantHost, cfg.QdrantGRPCPort, cfg.QdrantCollection)
	if err != nil {
		log.Fatalf("create qdrant client: %v", err)
	}
	qdrantCtx, qdrantCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer qdrantCancel()
	if err := qc.EnsureCollection(qdrantCtx); err != nil {
		// 嵌入模型不可用时向量检索已停用，Qdrant 不可达不再阻断服务启动
		log.Printf("WARN: ensure qdrant collection: %v (continuing without vector search)", err)
	}
	tracingInterceptor, err := temporalotel.NewTracingInterceptor(temporalotel.TracerOptions{})
	if err != nil {
		log.Fatalf("create temporal tracing interceptor: %v", err)
	}
	c, err := client.Dial(client.Options{
		HostPort:     cfg.TemporalHostPort,
		Namespace:    cfg.TemporalNamespace,
		Interceptors: []interceptor.ClientInterceptor{tracingInterceptor},
	})
	if err != nil {
		log.Fatalf("create temporal client: %v", err)
	}
	defer c.Close()

	// S2 混合编排（docs/plan/S2-混合编排.md §3.1）：agent-prod client，
	// 仅用于 CRON_QUEUE_MIGRATED=true 时 social-sched cron 的创建/取消。
	agentC, err := client.Dial(client.Options{
		HostPort:     cfg.TemporalHostPort,
		Namespace:    cfg.TemporalAgentNamespace,
		Interceptors: []interceptor.ClientInterceptor{tracingInterceptor},
	})
	if err != nil {
		log.Fatalf("create %s temporal client: %v", cfg.TemporalAgentNamespace, err)
	}
	defer agentC.Close()

	// S1 进化闭环：审批卡点 + Agent 绑定 + 进化单（docs/plan/S1-进化闭环.md §3.2）。
	// S2 §3.2：kind=workflow_signal 的审批单 approve/reject → Temporal Signal（agent-prod client）
	// S3 §3.1：brk 传入 approval（灰度联动：灰度恶化回滚时 open 该 skill 熔断）。
	notifyInst := notify.New()
	brk := circuitbreaker.NewRegistry(database)
	apvStore := approval.New(database, approval.LoadConfig(), notifyInst, approvalSignalSender{c: agentC}, brk)
	if err := apvStore.EnsureSchema(); err != nil {
		log.Fatalf("ensure approval schema: %v", err)
	}
	// S5 §3.4：灰度恶化回滚时自动停版本实例容器（deployctl version down 能力）。
	// 仅当 Docker socket 可用且 REPO_ROOT 指向 compose 项目时启用
	//（workflow-api 容器默认不挂载 socket → nil → 回滚留痕提示人工停）。
	if vm, verr := newVersionManager(database); verr == nil {
		apvStore = apvStore.WithVersionManager(vm)
		log.Printf("S5: 版本容器管理已启用（灰度回滚自动 version down）")
	} else {
		log.Printf("S5: 版本容器管理未启用（%v）——回滚时人工 deployctl version down", verr)
	}

	// S7 产品情报（docs/plan/S7-产品情报.md）：product 角色业务智能体（不写代码）
	productStore := product.NewStore(database.DB)
	if err := productStore.EnsureSchema(); err != nil {
		log.Fatalf("ensure product schema: %v", err)
	}
	// S9 统一工作项（docs/plan/S9-统一工作项流水线.md）：需求/ Bug/自进化统一货币 + 轻重缓急队列
	workitemStore := workitem.NewStore(database.DB)
	if err := workitemStore.EnsureSchema(); err != nil {
		log.Fatalf("ensure work_items schema: %v", err)
	}
	// S9 §7.8 项目注册（apps/ 目录 ↔ 项目 ↔ 可执行目标）
	projectStore := project.NewStore(database.DB)
	if err := projectStore.EnsureSchema(); err != nil {
		log.Fatalf("ensure projects schema: %v", err)
	}
	productAgent := product.NewAgent(productStore, qc, llmClient)

	deps := &appDeps{
		cfg:           cfg,
		database:      database,
		llm:           llmClient,
		qdrantClient:  qc,
		cragEngine:    crag.New(qc, llmClient),
		temporal:      c,
		temporalAgent: agentC,
		productStore:  productStore,
		productAgent:  productAgent,
		workitemStore: workitemStore,
		projectStore:  projectStore,
		apvStore:      apvStore,
		opsStore:      ops.NewStore(database.DB),
		notifier:      notifyInst,
	}

	// 统一认证（apps/auth）：本地 JWKS 验签；AUTH_URL 为空则不启用（兼容未部署 auth 的环境）
	var authVerifier *authn.Verifier
	if cfg.AuthURL != "" {
		authVerifier = authn.NewVerifier(cfg.AuthURL, cfg.AuthIssuer, cfg.AuthAudience)
	}
	requireAuth := func() gin.HandlerFunc {
		if authVerifier == nil {
			return func(ctx *gin.Context) { ctx.Next() }
		}
		return authVerifier.RequireAuth()
	}
	requireRole := func(roles ...string) gin.HandlerFunc {
		if authVerifier == nil {
			return func(ctx *gin.Context) { ctx.Next() }
		}
		return authVerifier.RequireRole(roles...)
	}

	r := gin.New()
	// gin v1.12 起 *gin.Context 不再默认把 Value()/Deadline() 等委托给请求 context，
	// 需显式开启回退；否则请求里的 otel span 对 temporal client（ExecuteWorkflow(ctx...)）
	// 不可见，api→go-worker 的链路会断开（StartWorkflow 变成新 root）。
	r.ContextWithFallback = true
	r.Use(internalotel.GinMiddleware())
	r.Use(otel.RecordGinMiddleware(database))
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"}, AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		AllowCredentials: true, MaxAge: 12 * time.Hour,
	}))

	r.GET("/health", func(ctx *gin.Context) { ctx.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	// ---- S1 审批 / Agent / 进化单 / OCR review 路由（approval 域自注册）----
	// P4-1：audit-logs 写入/查询路由鉴权（requireAuth，与 runs/intervention 同款）；
	// ai-flow 侧 HTTPAuditSink 经 pkg/workflow.Client 透传用户 JWT。
	approval.RegisterRoutes(r, apvStore, requireAuth())
	// ---- S8 模型管理：模型档案 CRUD + 应用（active）+ ai-flow 同步 ----
	approval.RegisterModelRoutes(r, apvStore, requireAuth())
	// OCR review-web 数据支撑（wi-1793175196-bd90）：审核记录列表（分页/筛选）+ 单条审核过程详情。
	registerOCRReviewRoutes(r, database, requireAuth())

	// ---- S5 运维中心「运行服务总览」（wi-1792321102）：服务列表/详情/重启/停止/回滚/改参 ----
	// API 容器经 Docker Engine API 操作容器（distroless 无 docker CLI）；
	// 不纳管（external）服务只读，写操作需 admin 角色 + audit_logs 留痕。
	var serviceOps *deploy.ServiceOps
	if so, serr := newServiceOps(database); serr == nil {
		serviceOps = so
		log.Printf("S5: 运行服务总览已启用（Docker Engine API）")
	} else {
		log.Printf("S5: 运行服务总览未启用（%v）", serr)
	}
	registerServiceOpsRoutes(r, serviceOps, database, requireAuth, requireRole)

	// ---- 产品板块「文档」：仓库 docs/ 目录的树形浏览/编辑/预览 + AI 生成（2026-10-19）----
	// DOCS_DIR 指向可写挂载点（compose 里 docs 单独 rw 挂载；REPO_ROOT 是只读的）。
	docsDir := strings.TrimSpace(os.Getenv("DOCS_DIR"))
	if docsDir == "" {
		if root := strings.TrimSpace(os.Getenv("REPO_ROOT")); root != "" {
			docsDir = filepath.Join(root, "docs")
		}
	}
	docsStore := docstore.New(docsDir)
	if docsStore == nil {
		log.Printf("文档：未启用（DOCS_DIR / REPO_ROOT 未配置）")
	} else {
		log.Printf("文档：docs 根 = %s", docsStore.Root())
	}
	registerDocsRoutes(r, docsStore, llmClient, database, requireAuth())

	// ---- Skill 网关 + Agent /runs/* + 干预舱 + 审批/灰度 ticker（server.go，行为无变更）----
	registerSkillGatewayRoutes(r, deps, brk, requireAuth, requireRole)

	// ---- 内容生产域（MCP→RAG→发布主链路：生成/社交/X/检索/采集/发布审批/状态总览）----
	registerContentRoutes(r, deps, requireAuth, requireRole)

	// 看板 AI（2026-11-20）：对话链路由 ai-flow 网关承接（core-ui nginx /api/ai-flow/ →
	// ai-flow:8134 /chat SSE），旧 board_ai（/boards/:id/ai-chat*）已删除。
	// /ai-execute 保留：ai-flow 侧栏的变更提案经前端确认后在这里落卡（能建/改卡，鉴权）。
	r.POST("/boards/:id/ai-execute", requireAuth(), boardAIExecute(database))

	// ---- 看板域（Board/List/Card/评论/执行记录/重跑/审批/思维导图）----
	registerBoardRoutes(r, deps, requireAuth, requireRole)

	// ---- S7 产品情报 + S9 统一工作项 + 项目注册 ----
	registerProductRoutes(r, deps, requireAuth, requireRole)

	// ---- S6 运维情报（error_logs 查询/统计/解决 + ops-reports）----
	registerOpsRoutes(r, deps, requireAuth, requireRole)

	serveAPI(cfg, r)
}
