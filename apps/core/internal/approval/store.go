// Package approval 是 S1 进化闭环的审批 / 灰度 / 审计核心
// （docs/plan/S1-进化闭环.md §3.2 / §3.4 / §3.5）。
//
// 职责：
//   - approvals / agent_bindings / agents_registry / evolve_tasks /
//     skill_circuit / interventions 六表访问层 + 幂等建表（EnsureSchema）；
//   - audit_logs 统一留痕（月分区初始化 + 每日过期分区清理，只增不删）；
//   - 审批路由（GET /approvals 等，无前缀风格与 legacy 一致）；
//   - 进程内 1 分钟 ticker：审批过期扫描（TickExpiry）+ 灰度状态机（TickGray）。
//
// 关键红线：不存在"默认通过"——审批单只会被人 approve / reject，
// 超时一律 expired（+ 升级通知），绝不自动放行。
package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/wetee/ai-workflow/internal/circuitbreaker"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
)

// Config 是审批 / 灰度 / 审计保留的运行配置（全部走 env）。
type Config struct {
	// 审批单过期（小时），过期 → expired + 升级通知（S1 §3.2）
	ApproveExpireHours int `env:"APPROVAL_EXPIRE_HOURS" envDefault:"24"`
	// 灰度窗口：时长（秒）或最小运行次数（先到者触发档位评估，S1 §3.4）
	GrayStageWindowSec int     `env:"GRAY_STAGE_WINDOW_SEC" envDefault:"1800"`
	GrayStageMinRuns   int     `env:"GRAY_STAGE_MIN_RUNS" envDefault:"200"`
	GrayFailRate       float64 `env:"GRAY_FAIL_RATE" envDefault:"0.2"`
	// audit_logs 保留天数（只 DROP 过期月分区，S1 §3.5）
	AuditLogRetentionDays int `env:"AUDIT_LOG_RETENTION_DAYS" envDefault:"180"`
}

// LoadConfig 解析 Config（失败 fatal，与其它 config 一致）。
func LoadConfig() *Config {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load approval config: %v", err)
	}
	return cfg
}

// SignalSender 向 Temporal workflow 发 Signal（S2 §3.2：board 审批操作 → workflow Signal）。
type SignalSender interface {
	SendSignal(ctx context.Context, workflowID, signalName string, payload any) error
}

// VersionStopper 版本实例管理（S5 §3.4）：启版本实例（Up，灰度跑新代码）+ 停版本实例（StopVersion，回滚）。
// 二者都是 *deploy.VersionManager 的能力；nil = S5 前人工操作（无 docker socket 的部署）。
type VersionStopper interface {
	StopVersion(ctx context.Context, skillName, version string) error
	Up(ctx context.Context, skillName, version, env string) (string, error)
}

// Store 是审批 / 进化链的数据访问 + 状态机入口。
type Store struct {
	db           *db.DB
	cfg          *Config
	notify       notify.Notifier
	signalSender SignalSender             // nil = 无 Signal 能力（如未接 Temporal 的测试环境）
	breaker      *circuitbreaker.Registry // nil = 无熔断联动（audit / evolve-driver）
	versionMgr   VersionStopper           // nil = 版本实例需人工停（S5 前）
}

// New 创建 Store。
// New 创建审批 Store。brk 为熔断器注册表（S3 §3.1 灰度联动：灰度恶化回滚时
// 同时 open 该 skill 熔断）；audit / evolve-driver 不触发灰度回滚，传 nil 即可。
func New(database *db.DB, cfg *Config, n notify.Notifier, sender SignalSender, brk *circuitbreaker.Registry) *Store {
	return &Store{db: database, cfg: cfg, notify: n, signalSender: sender, breaker: brk}
}

// WithVersionManager 挂接版本容器管理（S5：灰度恶化回滚时自动 version down）。
func (s *Store) WithVersionManager(v VersionStopper) *Store {
	s.versionMgr = v
	return s
}

// ensureTables 六表 DDL：与 apps/core/hacks/sql/approvals.sql 保持一致。
// audit_logs 为既有表（publish job 审计），此处只保证全新库存在（含 S1 列）。
var ensureTables = []string{
	`CREATE TABLE IF NOT EXISTS approvals (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  approval_no VARCHAR(32) NOT NULL UNIQUE,
  kind VARCHAR(32) NOT NULL,
  subject VARCHAR(255) NOT NULL,
  payload JSON NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  proposer VARCHAR(64) NOT NULL,
  approver VARCHAR(64),
  reason TEXT,
  expires_at DATETIME,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  decided_at DATETIME,
  KEY idx_approvals_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS audit_logs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  job_id BIGINT NULL,
  actor VARCHAR(64) NOT NULL,
  action VARCHAR(64) NOT NULL,
  target VARCHAR(255) NOT NULL DEFAULT '',
  note TEXT,
  detail JSON NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS agent_bindings (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  role VARCHAR(32) NOT NULL UNIQUE,
  agent_name VARCHAR(128) NOT NULL,
  version VARCHAR(32) NOT NULL DEFAULT 'latest',
  updated_by VARCHAR(64),
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS agents_registry (
  name VARCHAR(128) PRIMARY KEY,
  role VARCHAR(32) NOT NULL,
  version VARCHAR(32) NOT NULL DEFAULT 'latest',
  entrypoint VARCHAR(512) NOT NULL,
  container VARCHAR(64) NOT NULL,
  workdir_prefix VARCHAR(64) NOT NULL DEFAULT '/work',
  limits JSON NULL,
  permissions JSON NULL,
  config JSON NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS evolve_tasks (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  task_no VARCHAR(32) NOT NULL UNIQUE,
  source VARCHAR(32) NOT NULL,
  evidence JSON,
  target_name VARCHAR(128) NOT NULL,
  workdir VARCHAR(255),
  status VARCHAR(32) NOT NULL DEFAULT 'open',
  diff LONGTEXT,
  review JSON,
  test_report JSON,
  approval_id BIGINT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_evolve_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS skill_circuit (
  skill_name VARCHAR(128) PRIMARY KEY,
  state VARCHAR(16) NOT NULL DEFAULT 'close',
  consecutive_failures INT NOT NULL DEFAULT 0,
  opened_at DATETIME NULL,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS interventions (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  workflow_id VARCHAR(64) NOT NULL,
  run_id VARCHAR(64),
  kind VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'open',
  note TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  resolved_at DATETIME
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// model_profiles：模型管理（S8）目录。每行一个模型档案；active 标记唯一
	// 生效档（应用时应用层保证单 active）。base_url/api_key 是 OpenAI 兼容
	// 端点与密钥（api_key 仅服务端使用，列表接口脱敏）；consumers 标记该档
	// 适用的消费面（core=Go 核心 LLM，aiflow=ai-flow 网关，coder/reviewer=进化链，
	// 逗号分隔；空 = 全部）。embedding_model 仅 core 面有意义。
	`CREATE TABLE IF NOT EXISTS model_profiles (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(64) NOT NULL UNIQUE,
  provider VARCHAR(32) NOT NULL DEFAULT 'openai',
  model VARCHAR(128) NOT NULL,
  base_url VARCHAR(255) NOT NULL DEFAULT '',
  api_key VARCHAR(255) NOT NULL DEFAULT '',
  embedding_model VARCHAR(128) NOT NULL DEFAULT '',
  consumers VARCHAR(128) NOT NULL DEFAULT '',
  sync_agents TINYINT NOT NULL DEFAULT 1,
  active TINYINT NOT NULL DEFAULT 0,
  note VARCHAR(255) NOT NULL DEFAULT '',
  updated_by VARCHAR(64) NOT NULL DEFAULT '',
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// ensureColumns 兜底列（老表缺列时 ALTER ADD COLUMN，与 skills.sql 同一约定）。
var ensureColumns = map[string]map[string]string{}

// EnsureSchema 建表 + 补列 + audit_logs 月分区初始化。幂等，可在各进程启动时调用。
func (s *Store) EnsureSchema() error {
	for _, ddl := range ensureTables {
		if err := s.db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("ensure approval table: %w", err)
		}
	}
	for table, defs := range ensureColumns {
		for col, def := range defs {
			var cnt int64
			if err := s.db.Raw(
				"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
				table, col,
			).Scan(&cnt).Error; err != nil {
				return fmt.Errorf("inspect column %s.%s: %w", table, col, err)
			}
			if cnt == 0 {
				alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def)
				if err := s.db.Exec(alter).Error; err != nil {
					return fmt.Errorf("alter table %s: %w", table, err)
				}
			}
		}
	}
	return s.initAuditLogPartitions()
}

// Log 写一条审计日志（不可删，应用账号仅 SELECT + INSERT）。
// detail 可为 nil（无上下文）。返回写入结果 error：内部调用方（ticker/路由）忽略，
// P4-1 HTTP 入口 appendAuditLog 据此向调用方确认留痕成功。
func (s *Store) Log(ctx context.Context, actor, action, target string, detail any) error {
	var detailJSON string
	if detail != nil {
		b, err := json.Marshal(detail)
		if err == nil {
			detailJSON = string(b)
		}
	}
	entry := db.AuditLog{Actor: actor, Action: action, Target: target, Detail: detailJSON}
	if err := s.db.WithContext(ctx).Create(&entry).Error; err != nil {
		// 审计失败不能静默：留痕是 M1 的验收前提
		log.Printf("ERROR: write audit_log (%s %s %s): %v", actor, action, target, err)
		return err
	}
	return nil
}

// CreateIntervention 写一条干预记录（kind=manual 等，S3 干预舱提供管理入口）。
func (s *Store) CreateIntervention(ctx context.Context, workflowID, kind, note string) error {
	return s.db.WithContext(ctx).Create(&db.Intervention{
		WorkflowID: workflowID, Kind: kind, Status: "open", Note: note,
		CreatedAt: time.Now(),
	}).Error
}
