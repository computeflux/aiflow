// Package main is the health-patrol binary.
//
// health-patrol 是 S5 健康巡检与故障自愈进程（docs/plan/S5-软件接管与部署编排.md §3.3）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/deploy"
	"github.com/wetee/ai-workflow/internal/notify"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	RepoRoot      string `env:"REPO_ROOT" envDefault:"/work"`
	ComposeDir    string `env:"COMPOSE_DIR" envDefault:"/work/deploy"`
	HealthTimeout int    `env:"DEPLOY_HEALTH_TIMEOUT" envDefault:"30"`
	IntervalSec   int    `env:"HEALTH_PATROL_INTERVAL_SEC" envDefault:"600"`
	HealthPort    string `env:"HEALTH_PATROL_HEALTH_PORT" envDefault:"8127"`
	Actor         string `env:"HEALTH_PATROL_ACTOR" envDefault:"health-patrol"`
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
	store := deploy.NewStore(database)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure schema: %v", err)
	}
	if err := deploy.EnsureOpsTables(database); err != nil {
		log.Fatalf("ensure ops tables: %v", err)
	}
	notifier := notify.New()

	// health 探针（compose healthcheck）
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, "ok")
		})
		log.Printf("health-patrol health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	log.Printf("health-patrol started: interval=%ds health_timeout=%ds", cfg.IntervalSec, cfg.HealthTimeout)

	hooks := patrolHooks(cfg, database, notifier)
	state := &patrolState{
		fails:     map[string]int{},
		intervene: hooks.Intervene,
		notifyer:  notifier,
	}
	run := func() {
		ctx := context.Background()
		managed, err := store.ManagedComposeServices(ctx)
		if err != nil {
			log.Printf("WARN: list managed assets: %v", err)
			return
		}
		depl, err := deploy.NewDeployer(cfg.RepoRoot, cfg.ComposeDir, cfg.HealthTimeout, managed,
			hooks, cfg.Actor)
		if err != nil {
			log.Printf("WARN: new deployer: %v", err)
			return
		}
		state.patrol(ctx, depl)
	}

	// 启动即跑一轮（部署后无需等 10 分钟），之后按 interval 循环
	run()
	ticker := time.NewTicker(time.Duration(cfg.IntervalSec) * time.Second)
	for range ticker.C {
		run()
	}
}

// patrolState 巡检状态：每资产连续失败轮数（进程内；重启清零，可接受）。
type patrolState struct {
	mu        sync.Mutex
	fails     map[string]int
	intervene func(ctx context.Context, target, kind, note string) error
	notifyer  notify.Notifier
}

// patrol 跑一轮巡检（并行池 5）。
//
// 失败处理（S5 §3.3）：
//   - 第 1 轮失败：restart → 失败则 rebuild+restart；
//   - 第 2 轮失败：rebuild+restart；
//   - 第 3 轮起：升级（critical 告警 + intervention），skip（不循环自愈）。
func (s *patrolState) patrol(ctx context.Context, depl *deploy.Deployer) {
	svcs := depl.ManagedServices()
	if len(svcs) == 0 {
		log.Printf("patrol: 无纳管资产，跳过")
		return
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	healed, failed, healthy := 0, 0, 0
	for _, svc := range svcs {
		wg.Add(1)
		sem <- struct{}{}
		go func(svc *deploy.Service) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, reason := depl.CheckAsset(ctx, svc, 60*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				s.fails[svc.Name] = 0
				healthy++
				return
			}
			s.fails[svc.Name]++
			n := s.fails[svc.Name]
			log.Printf("patrol: %s 不健康（第 %d 轮）: %s", svc.Name, n, reason)

			var action, result string
			switch {
			case n == 1:
				ok, r := depl.Heal(ctx, svc, true) // restart → rebuild+restart
				action, result = healAction(r, ok)
			case n == 2:
				ok, r := depl.Heal(ctx, svc, false) // 直接 rebuild+restart
				action, result = healAction(r, ok)
			default:
				// 第 3 轮起：升级 + skip（不循环自愈）
				action, result = "skip", "fail"
				if n == 3 {
					s.intervene(ctx, svc.Name, "manual", "self_heal:escalated:fail")
					s.sendAlert(ctx, fmt.Sprintf("健康巡检升级 %s", svc.Name),
						fmt.Sprintf("连续 3 轮自愈失败（%s），建议人工介入（Matrix #ops-alerts 置顶）", reason), "critical")
					failed++
					return
				}
				// 第 4 轮起：只记日志，不再重复 intervention/告警
				log.Printf("patrol: %s 持续失败（第 %d 轮），已升级，等待人工介入", svc.Name, n)
				failed++
				return
			}
			if ok {
				healed++
				s.fails[svc.Name] = 0
			} else {
				failed++
			}
			s.intervene(ctx, svc.Name, "manual", fmt.Sprintf("self_heal:%s:%s", action, result))
			level := "warning"
			if !ok {
				level = "critical"
			}
			s.sendAlert(ctx, fmt.Sprintf("自愈%s %s（%s）", map[bool]string{true: "成功", false: "失败"}[ok], svc.Name, action),
				fmt.Sprintf("动作 %s → %s（%s）", action, result, reason), level)
		}(svc)
	}
	wg.Wait()
	log.Printf("patrol 完成: %d 健康 / %d 自愈恢复 / %d 失败", healthy, healed, failed)
}

// healAction 从自愈结果推导动作标签（restart / rebuild）与结果（ok / fail）。
func healAction(reason string, ok bool) (string, string) {
	if !ok {
		return "rebuild", "fail"
	}
	if reason == "restart 后恢复" {
		return "restart", "ok"
	}
	return "rebuild", "ok"
}

// sendAlert 发告警（no-op 通知器时静默）。
func (s *patrolState) sendAlert(ctx context.Context, title, body, level string) {
	if s.notifyer != nil {
		_ = s.notifyer.SendAlert(ctx, notify.Alert{Title: title, Body: body, Level: level})
	}
}

func patrolHooks(cfg *Config, database *gorm.DB, notifier notify.Notifier) deploy.Hooks {
	return deploy.Hooks{
		Audit: func(ctx context.Context, actor, action, target string, detail any) {
			auditLog(ctx, database, actor, action, target, detail)
		},
		Intervene: func(ctx context.Context, target, kind, note string) error {
			return database.WithContext(ctx).Create(&db.Intervention{
				WorkflowID: target, Kind: kind, Status: "open", Note: note, CreatedAt: time.Now(),
			}).Error
		},
		Alert: func(ctx context.Context, title, body, level string) {
			_ = notifier.SendAlert(ctx, notify.Alert{Title: title, Body: body, Level: level})
		},
	}
}

// auditLog 写一条审计日志（audit_logs 只增不删）。
func auditLog(ctx context.Context, database *gorm.DB, actor, action, target string, detail any) {
	var detailJSON string
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	if err := database.WithContext(ctx).Create(&db.AuditLog{
		Actor: actor, Action: action, Target: target, Detail: detailJSON,
	}).Error; err != nil {
		log.Printf("ERROR: write audit_log (%s %s %s): %v", actor, action, target, err)
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
		return nil, err
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	return gormDB, nil
}
