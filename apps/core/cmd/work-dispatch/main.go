// Package main is the work-dispatch binary.
//
// work-dispatch：S9 统一工作项流水线的调度进程（docs/plan/S9-统一工作项流水线.md §7.4/§7.5/§7.7）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/notify"
	"github.com/wetee/ai-workflow/internal/workitem"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	// MySQL
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	// 调度
	Enabled          bool   `env:"WORK_DISPATCH_ENABLED" envDefault:"true"`
	TriageInterval   string `env:"WORK_DISPATCH_TRIAGE_INTERVAL" envDefault:"15m"`
	DispatchInterval string `env:"WORK_DISPATCH_DISPATCH_INTERVAL" envDefault:"5m"`
	TriageWindow     string `env:"WORK_DISPATCH_TRIAGE_WINDOW" envDefault:"24h"`
	TriageMinCount   int    `env:"WORK_DISPATCH_TRIAGE_MIN_COUNT" envDefault:"1"`
	MaxInProgress    int    `env:"WORK_DISPATCH_MAX_IN_PROGRESS" envDefault:"1"`
	CardSync         bool   `env:"WORK_DISPATCH_CARD_SYNC" envDefault:"true"` // 每轮调度前把 ready_for_evolve 看板卡转为工作项入队（§7.6 看板入队）
	StarvationEvery  int    `env:"WORK_DISPATCH_STARVATION_EVERY" envDefault:"12"`
	StarvationWait   string `env:"WORK_DISPATCH_STARVATION_MIN_WAIT" envDefault:"24h"`
	ScoreWeights     string `env:"WORK_DISPATCH_SCORE_WEIGHTS" envDefault:"3,2,2,1"`
	RunOnStartup     bool   `env:"WORK_DISPATCH_RUN_ON_STARTUP" envDefault:"true"`
	HealthPort       string `env:"WORK_DISPATCH_HEALTH_PORT" envDefault:"8130"`

	// LLM（分诊；不可用 → 规则降级分诊）
	OpenAIAPIKey  string `env:"OPENAI_API_KEY" envDefault:""`
	OpenAIBaseURL string `env:"OPENAI_BASE_URL" envDefault:""`
	LLMModel      string `env:"LLM_MODEL" envDefault:""`
}

func main() {
	_ = godotenv.Load()
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// 一次性子命令：跑一轮即退出（tools profile / 手工触发）
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "triage", "dispatch", "feedback":
			runOnce(cfg, os.Args[1])
			return
		case "help", "-h", "--help":
			printUsage()
			return
		default:
			log.Fatalf("unknown subcommand %q（可用：triage | dispatch | feedback）", os.Args[1])
		}
	}

	runDaemon(cfg)
}

func runDaemon(cfg *Config) {
	database, err := openDB(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	store := workitem.NewStore(database)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure work_items schema: %v", err)
	}
	// S9.8 §7.10：确保 board_cards.target_name 列存在（workflow-api 的 AutoMigrate 也会补，
	// 但 work-dispatch 启动迁移独立运行，先补列避免启动顺序竞态；幂等）。
	if err := database.AutoMigrate(&db.BoardCard{}); err != nil {
		log.Fatalf("ensure board_cards schema: %v", err)
	}
	nt := notify.New()
	var llmClient *llm.Client
	if cfg.OpenAIAPIKey != "" {
		llmClient = llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, "", cfg.LLMModel)
	}
	triageCfg := workitem.TriageConfig{
		Window:   mustDuration(cfg.TriageWindow, 24*time.Hour),
		MinCount: cfg.TriageMinCount,
		Weights:  parseWeights(cfg.ScoreWeights),
	}
	dispatchCfg := workitem.DispatchConfig{
		MaxInProgress:     cfg.MaxInProgress,
		StarvationEvery:   cfg.StarvationEvery,
		StarvationMinWait: mustDuration(cfg.StarvationWait, 24*time.Hour),
		Weights:           parseWeights(cfg.ScoreWeights),
		LLM:               llmClient, // 启动前依赖检查（前置未完成 → 跳过，不自动领取）
	}

	// health 探针（compose healthcheck）
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
		// 手工触发（仅内部网络）
		mux.HandleFunc("/triage/once", func(w http.ResponseWriter, r *http.Request) {
			res, err := workitem.TriageErrorLogs(r.Context(), store, llmClient, triageCfg)
			writeJSON(w, res, err)
		})
		mux.HandleFunc("/dispatch/once", func(w http.ResponseWriter, r *http.Request) {
			res, err := workitem.Dispatch(r.Context(), store, dispatchCfg, 0)
			writeJSON(w, res, err)
		})
		mux.HandleFunc("/feedback/once", func(w http.ResponseWriter, r *http.Request) {
			res, err := workitem.Feedback(r.Context(), store, dispatchCfg.Weights, nt)
			writeJSON(w, res, err)
		})
		log.Printf("work-dispatch health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	if !cfg.Enabled {
		log.Printf("work-dispatch: disabled（WORK_DISPATCH_ENABLED=false），仅保留 health/手工端点")
		select {}
	}

	log.Printf("work-dispatch started: triage=%s dispatch=%s max_in_progress=%d weights=%s llm=%v",
		cfg.TriageInterval, cfg.DispatchInterval, cfg.MaxInProgress, cfg.ScoreWeights, llmClient != nil)

	if cfg.RunOnStartup {
		// S9 §7.4 重设：存量 Bug 工作项（无关联卡）一次性迁移到 Bug 看板（幂等）
		if n, err := workitem.MigrateBugItems(context.Background(), store); err != nil {
			log.Printf("WARN: migrate bug work items to Bug board cards: %v", err)
		} else if n > 0 {
			log.Printf("migrated %d bug work items to Bug board cards", n)
		}
		runTriage(context.Background(), store, llmClient, triageCfg)
		runDispatch(context.Background(), store, dispatchCfg, 0, cfg.CardSync)
		runFeedback(context.Background(), store, dispatchCfg.Weights, nt)
	}

	triageTicker := time.NewTicker(mustDuration(cfg.TriageInterval, 15*time.Minute))
	dispatchTicker := time.NewTicker(mustDuration(cfg.DispatchInterval, 5*time.Minute))
	feedbackTicker := time.NewTicker(mustDuration(cfg.DispatchInterval, 5*time.Minute))
	var round int
	for {
		select {
		case <-triageTicker.C:
			runTriage(context.Background(), store, llmClient, triageCfg)
		case <-dispatchTicker.C:
			round++
			runDispatch(context.Background(), store, dispatchCfg, round, cfg.CardSync)
		case <-feedbackTicker.C:
			runFeedback(context.Background(), store, dispatchCfg.Weights, nt)
		}
	}
}

// runOnce 跑一轮指定循环并打印结果（一次性子命令）。
func runOnce(cfg *Config, which string) {
	database, err := openDB(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	store := workitem.NewStore(database)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure work_items schema: %v", err)
	}
	nt := notify.New()
	var llmClient *llm.Client
	if cfg.OpenAIAPIKey != "" {
		llmClient = llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, "", cfg.LLMModel)
	}
	ctx := context.Background()
	triageCfg := workitem.TriageConfig{
		Window: mustDuration(cfg.TriageWindow, 24*time.Hour), MinCount: cfg.TriageMinCount, Weights: parseWeights(cfg.ScoreWeights),
	}
	dispatchCfg := workitem.DispatchConfig{
		MaxInProgress: cfg.MaxInProgress, StarvationEvery: cfg.StarvationEvery,
		StarvationMinWait: mustDuration(cfg.StarvationWait, 24*time.Hour), Weights: parseWeights(cfg.ScoreWeights),
		LLM: llmClient,
	}
	switch which {
	case "triage":
		res, err := workitem.TriageErrorLogs(ctx, store, llmClient, triageCfg)
		printResult("triage", res, err)
	case "dispatch":
		res, err := workitem.Dispatch(ctx, store, dispatchCfg, 0)
		printResult("dispatch", res, err)
	case "feedback":
		res, err := workitem.Feedback(ctx, store, dispatchCfg.Weights, nt)
		printResult("feedback", res, err)
	}
}

func runTriage(ctx context.Context, store *workitem.Store, llmClient *llm.Client, cfg workitem.TriageConfig) {
	res, err := workitem.TriageErrorLogs(ctx, store, llmClient, cfg)
	if err != nil {
		log.Printf("ERROR: triage round: %v", err)
		return
	}
	log.Printf("triage round: scanned=%d clusters=%d created=%d merged=%d dismissed=%d errors=%d",
		res.Scanned, res.Clusters, res.Created, res.Merged, res.Dismissed, len(res.Errors))
	for _, e := range res.Errors {
		log.Printf("  WARN: %s", e)
	}
}

func runDispatch(ctx context.Context, store *workitem.Store, cfg workitem.DispatchConfig, round int, cardSync bool) {
	// 看板入队（§7.6）：先把 ready_for_evolve 看板卡转为需求工作项入统一队列，再调度。
	if cardSync {
		cs, err := workitem.SyncCards(ctx, store, cfg.Weights)
		if err != nil {
			log.Printf("ERROR: card sync round %d: %v", round, err)
		} else {
			log.Printf("card sync round %d: scanned=%d created=%d errors=%d",
				round, cs.Scanned, cs.Created, len(cs.Errors))
			for _, e := range cs.Errors {
				log.Printf("  WARN: %s", e)
			}
		}
	}
	res, err := workitem.Dispatch(ctx, store, cfg, round)
	if err != nil {
		log.Printf("ERROR: dispatch round %d: %v", round, err)
		return
	}
	log.Printf("dispatch round %d: recomputed=%d dispatched=%v starved=%q errors=%d",
		round, res.Recomputed, res.Dispatched, res.Starved, len(res.Errors))
	for _, e := range res.Errors {
		log.Printf("  WARN: %s", e)
	}
}

func runFeedback(ctx context.Context, store *workitem.Store, w workitem.ScoreWeights, nt notify.Notifier) {
	res, err := workitem.Feedback(ctx, store, w, nt)
	if err != nil {
		log.Printf("ERROR: feedback round: %v", err)
		return
	}
	log.Printf("feedback round: scanned=%d shipped=%d rejected=%d expired=%d in_progress=%d errors=%d",
		res.Scanned, res.Shipped, res.Rejected, res.Expired, res.InProgress, len(res.Errors))
	for _, e := range res.Errors {
		log.Printf("  WARN: %s", e)
	}
	// S9 双向同步（§7.6）：镜像所有关联卡的卡状态 ← 工作项状态（含中间态，卡状态实时反映流水线）
	if syncRes, syncErr := workitem.SyncCardStatus(ctx, store); syncErr != nil {
		log.Printf("ERROR: card status sync: %v", syncErr)
	} else {
		log.Printf("card status sync: scanned=%d updated=%d errors=%d", syncRes.Scanned, syncRes.Updated, len(syncRes.Errors))
		for _, e := range syncRes.Errors {
			log.Printf("  WARN: %s", e)
		}
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
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	return gormDB, nil
}

// parseWeights 解析 "3,2,2,1" → ScoreWeights（severity,urgency,impact,frequency）。
func parseWeights(s string) workitem.ScoreWeights {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return workitem.DefaultWeights()
	}
	w := workitem.DefaultWeights()
	if v, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err == nil {
		w.Severity = v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
		w.Urgency = v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
		w.Impact = v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64); err == nil {
		w.Frequency = v
	}
	return w
}

func mustDuration(s string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "{\"error\":%q}", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = jsonEncode(w, v)
}

func printResult(label string, v any, err error) {
	if err != nil {
		log.Printf("%s: ERROR: %v", label, err)
		os.Exit(1)
	}
	log.Printf("%s: %s", label, mustJSON(v))
}

func printUsage() {
	fmt.Println("用法：work-dispatch [triage|dispatch|feedback]")
	fmt.Println("  （无子命令 = 守护进程：分诊/调度/回流三循环 + :8130 health/手工端点）")
}

func jsonEncode(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
