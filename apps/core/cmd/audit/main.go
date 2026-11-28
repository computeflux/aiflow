// Package main is the audit binary.
//
// audit：S1 审计巡检进程（docs/plan/S1-进化闭环.md §3.3）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/migration"
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

	// 巡检
	IntervalSec int     `env:"AUDIT_INTERVAL_SEC" envDefault:"1800"`
	WindowSec   int     `env:"AUDIT_WINDOW_SEC" envDefault:"1800"`
	FailRate    float64 `env:"AUDIT_FAIL_RATE" envDefault:"0.2"`
	MinRuns     int     `env:"AUDIT_MIN_RUNS" envDefault:"5"`
	MinErrors   int     `env:"AUDIT_MIN_ERRORS" envDefault:"5"`
	// S9 §7.6：审计发现 → 工作项（统一队列）而非直接建 evolve_task；false 回退旧行为（双轨过渡）。
	EmitWorkItems bool   `env:"AUDIT_EMIT_WORK_ITEMS" envDefault:"true"`
	ReportDir     string `env:"AUDIT_REPORT_DIR" envDefault:"/cache/audit"`

	// 审批 / 保留
	ApprovalExpireHours   int `env:"APPROVAL_EXPIRE_HOURS" envDefault:"24"`
	AuditLogRetentionDays int `env:"AUDIT_LOG_RETENTION_DAYS" envDefault:"180"`

	// LLM（可选；不可用 → 降级模式）
	OpenAIAPIKey  string `env:"OPENAI_API_KEY" envDefault:""`
	OpenAIBaseURL string `env:"OPENAI_BASE_URL" envDefault:""`
	LLMModel      string `env:"LLM_MODEL" envDefault:""`
	HealthPort    string `env:"AUDIT_HEALTH_PORT" envDefault:"8120"`

	// M4-1 会话保活（工作项 wi-1795489658-2d59）：账号池心跳巡检间隔（秒）。
	// 0 = 关闭（账号池还没接进审计时的降级开关；缺省 10 分钟一轮）。
	KeepAliveIntervalSec int `env:"KEEPALIVE_INTERVAL_SEC" envDefault:"600"`
}

type finding struct {
	Severity  string         `json:"severity"` // critical | warning
	SkillName string         `json:"skill_name,omitempty"`
	Metric    string         `json:"metric"`
	Value     string         `json:"value"`
	Evidence  map[string]any `json:"evidence"`
}

type report struct {
	GeneratedAt string    `json:"generated_at"`
	WindowSec   int       `json:"window_sec"`
	Findings    []finding `json:"findings"`
	Summary     string    `json:"summary,omitempty"`
	LLM         string    `json:"llm,omitempty"`
	Actions     []string  `json:"actions,omitempty"`
}

type skillAgg struct {
	total   int
	fails   int
	latency []int64
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
	apvCfg := &approval.Config{}
	if err := env.Parse(apvCfg); err != nil {
		log.Fatalf("failed to load approval config: %v", err)
	}
	// audit 不处理 workflow_signal 审批单（S2 桥在 api 侧），无 Signal 能力
	nt := notify.New()
	store := approval.New(database, apvCfg, nt, nil, nil)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure approval schema: %v", err)
	}
	// S9 §7.6：审计发现 → 工作项（进统一队列，与需求/Bug 公平竞争优先级）
	wiStore := workitem.NewStore(database.DB)
	if err := wiStore.EnsureSchema(); err != nil {
		log.Fatalf("ensure work_items schema: %v", err)
	}

	var llmClient *llm.Client
	if cfg.OpenAIAPIKey != "" {
		llmClient = llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, "", cfg.LLMModel)
	}

	// health 探针（compose healthcheck）
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, "ok")
		})
		log.Printf("audit health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	log.Printf("audit started: window=%ds interval=%ds fail_rate=%.2f min_runs=%d",
		cfg.WindowSec, cfg.IntervalSec, cfg.FailRate, cfg.MinRuns)

	var lastCleanupDay string
	run := func() {
		rep, err := runAudit(context.Background(), cfg, database, store, wiStore, nt, llmClient)
		if err != nil {
			log.Printf("ERROR: audit round: %v", err)
			return
		}
		log.Printf("audit round: %d findings (%d critical), actions=%v, report=%s",
			len(rep.Findings), countCritical(rep), rep.Actions, rep.GeneratedAt)
		today := time.Now().Format("2006-01-02")
		if lastCleanupDay != today {
			lastCleanupDay = today
			dropped, err := store.CleanupPartitions(context.Background())
			if err != nil {
				log.Printf("WARN: audit_logs partition cleanup: %v", err)
			} else if len(dropped) > 0 {
				log.Printf("audit_logs: dropped expired partitions %v", dropped)
			}
		}
	}

	// M4-1 会话保活：独立 ticker（比审计巡检更密），检测渠道登录态掉线 → need_login + 告警。
	if cfg.KeepAliveIntervalSec > 0 {
		ka, err := newKeepAlive(cfg, database, store, nt)
		if err != nil {
			log.Printf("WARN: 会话保活未启用: %v", err)
		} else {
			go func() {
				tick := func() {
					res, err := ka.Run(context.Background())
					if err != nil {
						log.Printf("WARN: keepalive tick: %v", err)
						return
					}
					if res.Dropped > 0 || len(res.Skipped) > 0 {
						log.Printf("keepalive: checked=%d dropped=%d alerted=%d skipped_channels=%v",
							res.Checked, res.Dropped, res.Alerted, res.Skipped)
					}
				}
				tick()
				kt := time.NewTicker(time.Duration(cfg.KeepAliveIntervalSec) * time.Second)
				for range kt.C {
					tick()
				}
			}()
			log.Printf("keepalive started: interval=%ds", cfg.KeepAliveIntervalSec)
		}
	}

	// 启动即跑一轮（部署后无需等 30 分钟），之后按 interval 循环
	run()
	ticker := time.NewTicker(time.Duration(cfg.IntervalSec) * time.Second)
	for range ticker.C {
		run()
	}
}

// newKeepAlive 装配 M4-1 的会话保活巡检器（工作项 wi-1795489658-2d59）。
//
// 判据来源是 social 账号池（social_accounts）的「最近一次成功登录时间」：登录回报链
// （apps/social 的 loginreport / pyreplay 的 AccountManagerReporter）每次都写 last_login_at，
// 所以「心跳」不需要额外的探针 —— 超过 KEEPALIVE_STALE_SEC 没有成功登录 = 会话已失效。
// 表缺失（social 还没上线）时建表并返回零结果，让「还没数据」是可见的零而不是报错。
func newKeepAlive(cfg *Config, database *db.DB, store *approval.Store, nt notify.Notifier) (*migration.KeepAlive, error) {
	if err := migration.EnsureSocialAccountTables(database.DB); err != nil {
		return nil, fmt.Errorf("ensure social_accounts: %w", err)
	}
	mcfg, err := migration.LoadEnvConfig()
	if err != nil {
		return nil, err
	}
	return migration.NewKeepAlive(
		store,
		migration.ListSocialAccounts(database.DB),
		migration.DropSocialAccountNeedLogin(database.DB),
		func(ctx context.Context, title, body, level string) error {
			return nt.SendAlert(ctx, notify.Alert{Title: title, Body: body, Level: level})
		},
		mcfg.KeepAliveConfig(),
	), nil
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

func countCritical(r *report) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == "critical" {
			n++
		}
	}
	return n
}

// runAudit 执行一轮巡检：采集 → 判定 → 报告 → 审批单/进化单。
func runAudit(ctx context.Context, cfg *Config, database *db.DB, store *approval.Store, wiStore *workitem.Store, nt notify.Notifier, llmClient *llm.Client) (*report, error) {
	now := time.Now()
	start := now.Add(-time.Duration(cfg.WindowSec) * time.Second)
	reportPath := filepath.Join(cfg.ReportDir, now.Format("20060102-150405")+".json")
	rep := &report{GeneratedAt: reportPath, WindowSec: cfg.WindowSec}

	// ---- 1. skill_runs：按 skill_name 聚合（近 30 分钟）
	var runs []struct {
		SkillName string `gorm:"column:skill_name"`
		Status    string `gorm:"column:status"`
		LatencyMs *int64 `gorm:"column:latency_ms"`
	}
	if err := database.WithContext(ctx).
		Raw("SELECT skill_name, status, latency_ms FROM skill_runs WHERE created_at >= ?", start).
		Scan(&runs).Error; err != nil {
		return nil, fmt.Errorf("query skill_runs: %w", err)
	}
	aggs := map[string]*skillAgg{}
	for _, r := range runs {
		a := aggs[r.SkillName]
		if a == nil {
			a = &skillAgg{}
			aggs[r.SkillName] = a
		}
		a.total++
		// 失败态：skillgateway.RunStatus* 为 ok/fail/timeout/rejected——非 ok 一律计失败
		// （2026-09-30 修：之前漏了 rejected，会把“全被限流拒绝”的 skill 判为健康）
		if r.Status != "ok" {
			a.fails++
		}
		if r.LatencyMs != nil {
			a.latency = append(a.latency, *r.LatencyMs)
		}
	}

	// skills 的 limits（timeout_sec）
	var skills []db.Skill
	if err := database.WithContext(ctx).Find(&skills).Error; err != nil {
		return nil, fmt.Errorf("query skills: %w", err)
	}
	timeouts := map[string]int64{}
	for _, s := range skills {
		timeouts[s.Name] = skillTimeoutSec(s.Limits)
	}

	for name, a := range aggs {
		if a.total >= cfg.MinRuns {
			rate := float64(a.fails) / float64(a.total)
			if rate > cfg.FailRate {
				rep.Findings = append(rep.Findings, finding{
					Severity: "critical", SkillName: name, Metric: "fail_rate",
					Value:    fmt.Sprintf("%.2f", rate),
					Evidence: map[string]any{"window_sec": cfg.WindowSec, "total": a.total, "fails": a.fails},
				})
			}
		}
		if t := timeouts[name]; t > 0 {
			if p99, ok := p99(a.latency); ok && p99 > t*800 { // timeout_sec × 0.8 → ms
				rep.Findings = append(rep.Findings, finding{
					Severity: "critical", SkillName: name, Metric: "p99_latency_ms",
					Value: fmt.Sprintf("%d", p99),
					Evidence: map[string]any{"window_sec": cfg.WindowSec, "runs": a.total,
						"p99_ms": p99, "threshold_ms": t * 800, "timeout_sec": t},
				})
			}
		}
	}

	// ---- 2. error_logs：新错误聚类（system+source，窗口内计数 ≥ 阈值）
	var errRows []struct {
		System string `gorm:"column:system"`
		Source string `gorm:"column:source"`
		Count  int64  `gorm:"column:cnt"`
	}
	if err := database.WithContext(ctx).
		Raw("SELECT `system`, source, COUNT(*) AS cnt FROM error_logs WHERE created_at >= ? GROUP BY `system`, source HAVING cnt >= ?", start, cfg.MinErrors).
		Scan(&errRows).Error; err != nil {
		return nil, fmt.Errorf("query error_logs: %w", err)
	}
	for _, e := range errRows {
		rep.Findings = append(rep.Findings, finding{
			Severity:  "warning",
			SkillName: e.System,
			Metric:    "error_count",
			Value:     fmt.Sprintf("%d", e.Count),
			Evidence:  map[string]any{"window_sec": cfg.WindowSec, "system": e.System, "source": e.Source, "count": e.Count},
		})
	}

	// ---- 2.5 S4 §3.3：audit 发现异常 → #ops-alerts（汇总 + 报告路径）
	if len(rep.Findings) > 0 {
		level := "warning"
		if countCritical(rep) > 0 {
			level = "critical"
		}
		var lines []string
		for _, f := range rep.Findings {
			lines = append(lines, fmt.Sprintf("- [%s] %s %s=%s", f.Severity, f.SkillName, f.Metric, f.Value))
		}
		_ = nt.SendAlert(ctx, notify.Alert{
			Title: fmt.Sprintf("audit 巡检：%d 项异常（%d critical）", len(rep.Findings), countCritical(rep)),
			Body:  strings.Join(lines, "\n") + "\nreport: " + reportPath,
			Level: level,
		})
	}

	// ---- 3. 严重问题 → 审批单（kind=rollback）+ evolve_tasks（source=patrol）
	// 只生成审批单，不自动改流量。
	critSkills := map[string]bool{}
	for _, f := range rep.Findings {
		if f.Severity == "critical" && f.SkillName != "" {
			critSkills[f.SkillName] = true
		}
	}
	for skillName := range critSkills {
		var pf []approval.PatrolFinding
		for _, f := range rep.Findings {
			if f.Severity == "critical" && f.SkillName == skillName {
				pf = append(pf, approval.PatrolFinding{
					Severity: f.Severity, Metric: f.Metric, Value: f.Value, Evidence: f.Evidence,
				})
			}
		}
		// S9：EmitWorkItems=true 时只建审批单（不建 evolve_task），改由工作项驱动进化链。
		created, err := store.CreatePatrolApproval(ctx, skillName, pf, rep.GeneratedAt, cfg.ApprovalExpireHours, !cfg.EmitWorkItems)
		if err != nil {
			log.Printf("WARN: create patrol approval for %s: %v", skillName, err)
			continue
		}
		if !created {
			continue
		}
		if cfg.EmitWorkItems {
			if wiNo, werr := createAuditWorkItem(ctx, wiStore, skillName, pf, rep.GeneratedAt); werr != nil {
				log.Printf("WARN: create audit work item for %s: %v", skillName, werr)
				rep.Actions = append(rep.Actions, "approval(rollback) created for "+skillName)
			} else {
				rep.Actions = append(rep.Actions, "approval(rollback) + work_item "+wiNo+" for "+skillName)
			}
		} else {
			rep.Actions = append(rep.Actions, "approval(rollback) + evolve_task created for "+skillName)
		}
	}

	// ---- 4. LLM 归纳（降级模式：不可用只出指标告警，不阻塞）
	if llmClient != nil {
		ctxL, cancel := context.WithTimeout(ctx, 60*time.Second)
		summary, err := llmClient.ChatCompletion(ctxL,
			"你是运维审计助手，用 1-2 句中文总结以下巡检发现（JSON）：",
			string(mustJSON(rep.Findings)))
		cancel()
		if err != nil {
			rep.LLM = "unavailable"
			log.Printf("WARN: llm summary unavailable (degraded mode): %v", err)
		} else {
			rep.Summary = strings.TrimSpace(summary)
		}
	} else {
		rep.LLM = "disabled"
	}

	// ---- 5. 写报告文件
	os.MkdirAll(cfg.ReportDir, 0o755)
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(reportPath, b, 0o644); err != nil {
		return nil, fmt.Errorf("write report %s: %w", reportPath, err)
	}
	return rep, nil
}

func p99(lats []int64) (int64, bool) {
	if len(lats) == 0 {
		return 0, false
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	idx := int(math.Ceil(0.99*float64(len(lats)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(lats) {
		idx = len(lats) - 1
	}
	return lats[idx], true
}

func skillTimeoutSec(limitsJSON string) int64 {
	if limitsJSON == "" {
		return 0
	}
	var limits struct {
		TimeoutSec int64 `json:"timeout_sec"`
	}
	if err := json.Unmarshal([]byte(limitsJSON), &limits); err != nil {
		return 0
	}
	return limits.TimeoutSec
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// createAuditWorkItem 为审计发现的严重问题建工作项（S9 §7.6）：
// kind=bug, source=audit, target=app（技能并入 app，S9.8 §7.10）；算分入队（queued），由调度器领取驱动 S1 链。
// 去重：同 skill 已有非终态工作项（上一轮单还在途/卡审）→ 复用不重复建
// （审批单去重只覆盖 pending rollback 单；旧单过期后若无此层，同 skill 会双工作项）。
func createAuditWorkItem(ctx context.Context, wiStore *workitem.Store, skillName string, findings []approval.PatrolFinding, reportFile string) (string, error) {
	if existing, err := wiStore.FindOpenByTarget(ctx, skillName); err == nil && existing != "" {
		log.Printf("audit: work item deduped for %s (existing %s)", skillName, existing)
		return existing, nil
	}
	var desc strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&desc, "- [%s] %s=%s\n", f.Severity, f.Metric, f.Value)
	}
	item := &db.WorkItem{
		Kind:        workitem.KindBug,
		Source:      workitem.SourceAudit,
		SourceRef:   reportFile,
		TargetName:  skillName,
		Title:       fmt.Sprintf("审计：%s 指标超阈值", skillName),
		Description: "审计巡检发现（报告 " + reportFile + "）：\n" + desc.String(),
		Severity:    3,
		Urgency:     2,
		Impact:      3,
		Frequency:   1,
		Status:      workitem.StatusTriaged,
	}
	if err := wiStore.Create(ctx, item); err != nil {
		return "", err
	}
	_ = wiStore.UpdateScores(ctx, item.No, item.Severity, item.Urgency, item.Impact, item.Frequency, workitem.DefaultWeights())
	if err := wiStore.UpdateStatus(ctx, item.No, workitem.StatusQueued); err != nil {
		return "", err
	}
	return item.No, nil
}
