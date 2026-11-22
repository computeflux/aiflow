// Package main is the mk-judge binary.
//
// eval.go — mk-judge -eval 子命令（后续卡实现：判断质量评估）。
// 独立文件（P2 串行红线：避免多张卡改同一个 main.go）。
//
// mk-judge：营销判断服务 HTTP 入口（docs/marketing/plan/P2-判断服务地基.md MK-16）。
// gold.go — mk-judge -gold-* 子命令（后续卡实现：金标回归 run/diff/report）。
// 独立文件（P2 串行红线：避免多张卡改同一个 main.go）。
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/caarlos0/env/v11"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/marketing"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	// MySQL
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	// 服务
	HealthPort string `env:"MK_JUDGE_HEALTH_PORT" envDefault:"8131"`

	// LLM（判断主流程；不可用 → /judge 硬失败 502，不假装智能）
	OpenAIAPIKey  string `env:"OPENAI_API_KEY" envDefault:""`
	OpenAIBaseURL string `env:"OPENAI_BASE_URL" envDefault:""`
	LLMModel      string `env:"LLM_MODEL" envDefault:""`
}

// judgeHTTPRequest 是 POST /judge 的 body：JudgeRequest 原料 + 顶层 region/product
// （★ 规格 §2/§7：所有判断必须带 region/product；缺 region → 400）。
type judgeHTTPRequest struct {
	Region  string `json:"region"` // intl | cn（必填）
	Product string `json:"product"`
	marketing.JudgeRequest
}

func main() {
	_ = godotenv.Load()
	// 一次性子命令：flag 分发（seed.go / eval.go / gold.go）。
	// 必须在 env.Parse 之前：子命令（如 -seed 离线跑批）不依赖 MySQL 配置，
	// 本地无 .env 时也能跑（子命令自己按需 godotenv.Load + 读 LLM 变量）。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-seed":
			runSeed()
		case "-eval":
			runEval()
		case "-gold-run", "-gold-diff", "-gold-report":
			runGold(os.Args[1])
		case "help", "-h", "--help":
			printUsage()
		default:
			log.Fatalf("unknown subcommand %q（可用：-seed | -eval | -gold-run | -gold-diff | -gold-report）", os.Args[1])
		}
		return
	}

	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	database, err := openDB(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	store := marketing.NewStore(database)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure mk_judgments schema: %v", err)
	}
	var (
		llmClient marketing.LLMClient
		modelName string
	)
	if cfg.OpenAIAPIKey != "" {
		client := llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, "", cfg.LLMModel)
		llmClient = client
		modelName = client.Model()
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.POST("/judge", handleJudge(store, llmClient, modelName))
	r.GET("/judge/:id", handleGetJudgment(store))
	r.POST("/replay/:id", handleReplay(store, llmClient, modelName))

	log.Printf("mk-judge listening on :%s (llm=%v model=%s)", cfg.HealthPort, llmClient != nil, modelName)
	log.Fatal(http.ListenAndServe(":"+cfg.HealthPort, r))
}

// handleJudge：POST /judge 判断主流程（MK-14）。缺 region → 400。
//
// 注意：外层 Region/Product（带 json tag）遮蔽了内嵌 JudgeRequest 的同名字段，
// body 的 region/product 只会落到外层字段；而 marketing.Judge 组装上下文用的是
// req.Context（ContextInput，judge.go AssembleContext(req.Context)），Region/Product/Kind
// 都从中取。故这里必须把顶层字段同步进 Context，否则只传顶层 region 时 Context 为空
// → AssembleContext 报「非法 region/product/kind」；或 context 里另填的 region 与顶层
// 不一致 → 落库 region 与 snapshot/纯度校验走两套（串味检查失效）。
func handleJudge(store *marketing.Store, llmClient marketing.LLMClient, modelName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req judgeHTTPRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
			return
		}
		if req.Region == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "region 必填（intl | cn）"})
			return
		}
		if req.JudgeRequest.Kind == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kind 必填（judge_lead 等，见 JudgeRequest.Kind）"})
			return
		}
		// 顶层字段为权威值（规格 §2/§7）：无条件覆盖 Context，保证 snapshot/纯度校验
		// （用 Context 组装）与落库记录（用 JudgeRequest 字段）同源；若「只补空」，
		// body 同时给 region=intl 与 context.region=cn 时会造出两套数据（snapshot 按 cn
		// 走、落库却是 intl），region 隔离校验与复盘结论失真。
		req.JudgeRequest.Context.Region = req.Region
		req.JudgeRequest.Context.Product = req.Product
		req.JudgeRequest.Context.Kind = req.JudgeRequest.Kind
		// JudgeRequest.Region/Product 供落库记录用（外层字段遮蔽，body 收不到，须显式赋值）。
		req.JudgeRequest.Region = req.Region
		req.JudgeRequest.Product = req.Product
		// Model 未指定时用实际配置的模型（落库标记「实际用的哪个模型」）。
		if req.JudgeRequest.Model == "" {
			req.JudgeRequest.Model = modelName
		}
		if llmClient == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "LLM 未配置（OPENAI_API_KEY 为空）"})
			return
		}
		j, err := marketing.Judge(c.Request.Context(), store, llmClient, req.JudgeRequest)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, j)
	}
}

// handleGetJudgment：GET /judge/:id 回放——返回该判断的上下文快照 snapshot。
func handleGetJudgment(store *marketing.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id 非法"})
			return
		}
		j, err := store.ByID(c.Request.Context(), uint(id))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "judgment 不存在"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id":       j.ID,
			"verdict":  j.Verdict,
			"reason":   j.Reason,
			"snapshot": j.ContextSnapshot,
		})
	}
}

// handleReplay：POST /replay/:id 用原 snapshot 重跑（当前 prompt 版本 + 实际模型），便于对比 prompt 改动。
// 重跑结果同样落库（新记录，prompt_version 为当前版本、model 为实际模型），原记录不动。
func handleReplay(store *marketing.Store, llmClient marketing.LLMClient, modelName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id 非法"})
			return
		}
		orig, err := store.ByID(c.Request.Context(), uint(id))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "judgment 不存在"})
			return
		}
		if llmClient == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "LLM 未配置（OPENAI_API_KEY 为空）"})
			return
		}
		// 用原 snapshot 直接重跑（snapshot 本身就是当时的 user prompt，直接复用，
		// 不反序列化回 ContextInput——避免丢原料）；prompt 用当前版本、model 用实际模型。
		j, err := marketing.JudgeFromSnapshot(c.Request.Context(), store, llmClient, *orig, modelName)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"original_id": orig.ID,
			"original":    gin.H{"verdict": orig.Verdict, "reason": orig.Reason, "prompt_version": orig.PromptVersion, "model": orig.Model},
			"replayed":    j,
		})
	}
}

func openDB(cfg *Config) (*gorm.DB, error) {
	gormDB, err := gorm.Open(mysql.Open(fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		cfg.MySQLUser, cfg.MySQLPassword, cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLDatabase,
	)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db: %w", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	return gormDB, nil
}

func printUsage() {
	fmt.Println("用法：mk-judge [-seed | -eval | -gold-run | -gold-diff | -gold-report]")
	fmt.Println("  （无子命令 = HTTP 服务：:8131 /healthz + /judge + /judge/:id + /replay/:id，仅内部网络）")
}
