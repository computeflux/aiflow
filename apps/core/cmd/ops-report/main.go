// Package main is the ops-report binary.
//
// ops-report：S6 运维情报进程（docs/plan/S6-运维情报.md §3.1/§3.2）。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/ops"
	"go.temporal.io/sdk/client"
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

	// Temporal（可选；不可用 → 概览降级为 0 + note）
	TemporalHostPort  string `env:"TEMPORAL_HOST_PORT" envDefault:""`
	TemporalNamespace string `env:"TEMPORAL_NAMESPACE" envDefault:"default"`

	// LLM（可选；不可用 → 降级为纯数据表 / 不出建议）
	OpenAIAPIKey  string `env:"OPENAI_API_KEY" envDefault:""`
	OpenAIBaseURL string `env:"OPENAI_BASE_URL" envDefault:""`
	LLMModel      string `env:"LLM_MODEL" envDefault:""`

	// 仓库根（git 热点 / TODO 扫描用；容器挂载 ../../../:/work）
	RepoRoot string `env:"OPS_REPORT_REPO_ROOT" envDefault:"/work"`

	// 运行控制
	RunOnStartup bool   `env:"OPS_REPORT_RUN_ON_STARTUP" envDefault:"true"`
	HealthPort   string `env:"OPS_REPORT_HEALTH_PORT" envDefault:"8128"`
}

func main() {
	_ = godotenv.Load()
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	database, err := openDB(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	store := ops.NewStore(database.DB)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure ops_reports schema: %v", err)
	}

	var llmClient *llm.Client
	if cfg.OpenAIAPIKey != "" {
		llmClient = llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, "", cfg.LLMModel)
	}

	var temporal client.Client
	if cfg.TemporalHostPort != "" {
		tc, err := client.Dial(client.Options{HostPort: cfg.TemporalHostPort, Namespace: cfg.TemporalNamespace})
		if err != nil {
			log.Printf("WARN: temporal dial failed (degraded): %v", err)
		} else {
			temporal = tc
			defer tc.Close()
		}
	}

	// health 探针（compose healthcheck，仅内部网络）
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("ok"))
		})
		log.Printf("ops-report health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	log.Printf("ops-report started: llm=%v temporal=%v repo=%s run_on_startup=%v",
		cfg.OpenAIAPIKey != "", temporal != nil, cfg.RepoRoot, cfg.RunOnStartup)

	// 启动即跑一轮（幂等 upsert，重启不产生重复行；非周一不出周报/建议），之后每日 08:00 循环
	if cfg.RunOnStartup {
		runAll(cfg, store, database, llmClient, temporal)
	}
	for {
		next := next8AM(time.Now())
		time.Sleep(time.Until(next))
		runAll(cfg, store, database, llmClient, temporal)
	}
}

// runAll 跑一轮：日报必出；周一加出周报 + 升级建议。
func runAll(cfg *Config, store *ops.Store, database *db.DB, llmClient *llm.Client, temporal client.Client) {
	ctx := context.Background()

	if rep, err := ops.GenerateDaily(ctx, store, database, llmClient, temporal, cfg.TemporalNamespace); err != nil {
		log.Printf("ERROR: daily report: %v", err)
	} else {
		log.Printf("daily report: id=%d date=%s", rep.ID, rep.ReportDate.Format("2006-01-02"))
	}

	if time.Now().Weekday() == time.Monday {
		if rep, err := ops.GenerateWeekly(ctx, store, database, llmClient, temporal, cfg.TemporalNamespace); err != nil {
			log.Printf("ERROR: weekly report: %v", err)
		} else {
			log.Printf("weekly report: id=%d date=%s", rep.ID, rep.ReportDate.Format("2006-01-02"))
		}
		if rep, err := ops.GenerateSuggestions(ctx, store, database, llmClient, temporal, cfg.TemporalNamespace, cfg.RepoRoot); err != nil {
			log.Printf("ERROR: upgrade suggestions: %v", err)
		} else {
			log.Printf("upgrade suggestions: id=%d date=%s", rep.ID, rep.ReportDate.Format("2006-01-02"))
		}
	}
}

// next8AM 返回下一个本地时间 08:00（若今天 08:00 已过则取明天）。
func next8AM(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 8, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func openDB(cfg *Config) (*db.DB, error) {
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
	return &db.DB{DB: gormDB}, nil
}
