// Package main is the watchdog binary.
//
// watchdog：S3 干预舱死循环检测进程（docs/plan/S3-干预舱.md §3.2）。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"go.temporal.io/sdk/client"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
	"github.com/wetee/ai-workflow/internal/temporalscan"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	// MySQL
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	// Temporal（两个 namespace 都要扫）
	TemporalHostPort       string `env:"TEMPORAL_HOST_PORT" envDefault:"127.0.0.1:7233"`
	TemporalNamespace      string `env:"TEMPORAL_NAMESPACE" envDefault:"default"`
	TemporalAgentNamespace string `env:"TEMPORAL_AGENT_NAMESPACE" envDefault:"agent-prod"`

	// 死循环判定
	IntervalSec       int    `env:"WATCHDOG_INTERVAL_SEC" envDefault:"60"`
	MaxConsecFailures int    `env:"WATCHDOG_MAX_CONSEC_FAILURES" envDefault:"10"`
	MaxEvents         int    `env:"WATCHDOG_MAX_EVENTS" envDefault:"5000"`
	HealthPort        string `env:"WATCHDOG_HEALTH_PORT" envDefault:"8125"`
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

	defC, err := client.Dial(client.Options{HostPort: cfg.TemporalHostPort, Namespace: cfg.TemporalNamespace})
	if err != nil {
		log.Fatalf("create %s temporal client: %v", cfg.TemporalNamespace, err)
	}
	defer defC.Close()
	agentC, err := client.Dial(client.Options{HostPort: cfg.TemporalHostPort, Namespace: cfg.TemporalAgentNamespace})
	if err != nil {
		log.Fatalf("create %s temporal client: %v", cfg.TemporalAgentNamespace, err)
	}
	defer agentC.Close()

	notifier := notify.New()

	// health 探针（compose healthcheck）
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, "ok")
		})
		log.Printf("watchdog health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	log.Printf("watchdog started: interval=%ds max_consec_failures=%d max_events=%d",
		cfg.IntervalSec, cfg.MaxConsecFailures, cfg.MaxEvents)

	run := func() {
		ctx := context.Background()
		type clientNS struct {
			c  client.Client
			ns string
		}
		for _, p := range []clientNS{
			{agentC, cfg.TemporalAgentNamespace},
			{defC, cfg.TemporalNamespace},
		} {
			scans, err := temporalscan.ScanRunning(ctx, p.c, p.ns, 50)
			if err != nil {
				log.Printf("WARN: scan %s: %v", p.ns, err)
				continue
			}
			for _, s := range scans {
				if !isLoop(s, cfg) {
					continue
				}
				reason := loopReason(s, cfg)
				created, cerr := createLoopDetect(ctx, database, p.ns, s, reason)
				if cerr != nil {
					log.Printf("WARN: create loop_detect for %s/%s: %v", p.ns, s.WorkflowID, cerr)
					continue
				}
				if created {
					notifier.SendAlert(ctx, notify.Alert{
						Title: "watchdog 死循环检测",
						Body:  fmt.Sprintf("%s/%s: %s（last_error=%s）", p.ns, s.WorkflowID, reason, s.LastError),
						Level: "critical",
					})
				}
			}
		}
	}

	// 启动即跑一轮（部署后无需等 1 分钟），之后按 interval 循环
	run()
	ticker := time.NewTicker(time.Duration(cfg.IntervalSec) * time.Second)
	for range ticker.C {
		run()
	}
}

// isLoop 判定是否死循环：同一 activity 连续失败 ≥ 阈值，或 event 数 > 阈值。
func isLoop(s temporalscan.WorkflowScan, cfg *Config) bool {
	return s.MaxConsecutiveActivityFailures >= cfg.MaxConsecFailures || s.EventCount > cfg.MaxEvents
}

// loopReason 生成判定原因（note + 告警正文）。
func loopReason(s temporalscan.WorkflowScan, cfg *Config) string {
	if s.MaxConsecutiveActivityFailures >= cfg.MaxConsecFailures {
		return fmt.Sprintf("activity 连续失败 %d 次", s.MaxConsecutiveActivityFailures)
	}
	return fmt.Sprintf("event 数 %d 超限（阈值 %d）", s.EventCount, cfg.MaxEvents)
}

// createLoopDetect 写一条 loop_detect intervention（按 workflow_id 去重 open 记录）。
// 返回 created（本次是否新建）。
func createLoopDetect(ctx context.Context, database *db.DB, namespace string, s temporalscan.WorkflowScan, reason string) (bool, error) {
	var dup int64
	if err := database.WithContext(ctx).Model(&db.Intervention{}).
		Where("workflow_id = ? AND kind = ? AND status = ?", s.WorkflowID, "loop_detect", "open").
		Count(&dup).Error; err != nil {
		return false, fmt.Errorf("dedup check: %w", err)
	}
	if dup > 0 {
		return false, nil
	}
	note := fmt.Sprintf("%s: %s（events=%d, last_error=%s）", namespace, reason, s.EventCount, s.LastError)
	if err := database.WithContext(ctx).Create(&db.Intervention{
		WorkflowID: s.WorkflowID,
		Kind:       "loop_detect",
		Status:     "open",
		Note:       note,
		CreatedAt:  time.Now(),
	}).Error; err != nil {
		return false, fmt.Errorf("create intervention: %w", err)
	}
	log.Printf("loop_detect: %s/%s %s", namespace, s.WorkflowID, reason)
	return true, nil
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
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	return &db.DB{DB: gormDB}, nil
}
