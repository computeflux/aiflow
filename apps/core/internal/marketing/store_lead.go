// Package marketing 是营销判断服务（docs/marketing/implementation-spec.md）：
// 本文件（MK-19）提供 mk_leads / mk_actions 两张表的访问层（Store）。
//
// 一卡一组文件（P2 串行红线）：
//
//	MK-10 → model.go（域模型 + 枚举校验）
//	MK-11 → store_judgment.go（mk_judgments 表 + store）
//	MK-19 → store_lead.go（mk_leads / mk_actions 表 + store，本文件）
//
// 本阶段（P2）两张表只写不读：提供 UpsertLead（幂等）、CreateAction、
// UpdateActionStatus；不接入发送 / 限流（M6 才需要多机，规格 §9）。
package marketing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---- 表名（不依赖 GORM 命名策略）----

func (Lead) TableName() string   { return "mk_leads" }
func (Action) TableName() string { return "mk_actions" }

// ---- 建表 DDL（与 apps/core/hacks/sql/marketing_leads.sql 保持一致）----
// 双轨：SQL 文件给 hacks/bootstrap.sh，ensureDDL 给服务自启兜底。

var ensureLeadsDDL = `CREATE TABLE IF NOT EXISTS mk_leads (
  id           BIGINT AUTO_INCREMENT PRIMARY KEY,
  region       VARCHAR(16) NOT NULL,
  product      VARCHAR(16) NOT NULL DEFAULT 'network',
  source       VARCHAR(32) NOT NULL,
  source_ref   VARCHAR(255) NOT NULL,
  url          VARCHAR(512) NOT NULL DEFAULT '',
  author       VARCHAR(128) NOT NULL DEFAULT '',
  excerpt      TEXT,
  is_real_need TINYINT NOT NULL DEFAULT 0,
  need_type    VARCHAR(32) NOT NULL DEFAULT 'noise',
  intervene    VARCHAR(16) NOT NULL DEFAULT 'monitor',
  pain         VARCHAR(32) NOT NULL DEFAULT '',
  score        INT NOT NULL DEFAULT 0,
  confidence   DECIMAL(4,3) NOT NULL DEFAULT 0,
  judgment_id  BIGINT NOT NULL DEFAULT 0,
  idem_key     VARCHAR(128) NOT NULL,
  status       VARCHAR(32) NOT NULL DEFAULT 'new',
  found_at     DATETIME NOT NULL,
  UNIQUE KEY uk_idem (idem_key),
  KEY idx_region_status (region, status, score DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

var ensureActionsDDL = `CREATE TABLE IF NOT EXISTS mk_actions (
  id                     BIGINT AUTO_INCREMENT PRIMARY KEY,
  region                 VARCHAR(16) NOT NULL,
  product                VARCHAR(16) NOT NULL DEFAULT 'network',
  lead_id                BIGINT NOT NULL DEFAULT 0,
  agent                  VARCHAR(8) NOT NULL,
  kind                   VARCHAR(16) NOT NULL,
  channel                VARCHAR(48) NOT NULL,
  account                VARCHAR(64) NOT NULL,
  target_url             VARCHAR(512) NOT NULL DEFAULT '',
  body                   LONGTEXT NOT NULL,
  draft_judgment_id      BIGINT NOT NULL DEFAULT 0,
  critique_judgment_id   BIGINT NOT NULL DEFAULT 0,
  compliance_judgment_id BIGINT NOT NULL DEFAULT 0,
  status                 VARCHAR(32) NOT NULL DEFAULT 'draft',
  idem_key               VARCHAR(128) NOT NULL,
  created_at             DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_idem (idem_key),
  KEY idx_region_status (region, status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

// ---- LeadStore：mk_leads 表访问层 ----

// LeadStore 是 mk_leads 表访问层（直接取 *gorm.DB，与 workitem.Store / product.Store 同风格）。
type LeadStore struct {
	database *gorm.DB
}

// NewLeadStore 构造 mk_leads 表访问层。
func NewLeadStore(database *gorm.DB) *LeadStore { return &LeadStore{database: database} }

// EnsureSchema 建 mk_leads / mk_actions 两表。幂等，可在进程启动时调用。
func (s *LeadStore) EnsureSchema() error {
	if err := s.database.Exec(ensureLeadsDDL).Error; err != nil {
		return fmt.Errorf("ensure mk_leads table: %w", err)
	}
	if err := s.database.Exec(ensureActionsDDL).Error; err != nil {
		return fmt.Errorf("ensure mk_actions table: %w", err)
	}
	return nil
}

// UpsertLead 按 idem_key 幂等写入线索（单条 INSERT ... ON DUPLICATE KEY UPDATE，无预查）：
// 首次插入（GORM 由 LastInsertId 回填 l.ID，status 取默认 new）；idem_key 已存在时
// 只刷新判断结果字段（is_real_need / need_type / intervene / pain / score / confidence /
// judgment_id / found_at），来源字段（source / source_ref / url / author / excerpt）与
// status 均保持既有值——status 只由人工 / 后续流程的显式接口变更，重复抓取 / 重试不得覆盖。
// 返回 (created, err)：created 由写入结果判断——MySQL ODKU 插入 RowsAffected=1、
// 更新=2、无变化=0，故 created = (RowsAffected == 1)；并发安全由唯一键 uk_idem 保证。
func (s *LeadStore) UpsertLead(ctx context.Context, l *Lead) (created bool, err error) {
	if err := l.Validate(); err != nil {
		return false, err
	}
	if strings.TrimSpace(l.IdemKey) == "" {
		return false, fmt.Errorf("marketing: lead idem_key 不能为空（uk_idem 幂等键）")
	}
	if l.FoundAt.IsZero() {
		l.FoundAt = time.Now()
	}
	if l.Status == "" {
		l.Status = LeadStatusNew
	}
	// l.ID 保持零值：GORM 不会把零值自增主键写进 INSERT（若回填非零 id，
	// 显式主键不在冲突目标 idem_key 内，sqlite/Postgres 方言会直接报主键冲突）；
	// 插入成功后 GORM 自动回填 l.ID。
	// 注意：DoUpdates 不含 status——更新分支不刷新状态，避免把人工推进到
	// reviewed / converted / discarded 的状态静默覆盖回 new（状态只由显式接口变更）。
	res := s.database.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "idem_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"is_real_need", "need_type", "intervene", "pain",
			"score", "confidence", "judgment_id", "found_at",
		}),
	}).Create(l)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ---- ActionStore：mk_actions 表访问层 ----

// ActionStore 是 mk_actions 表访问层。
type ActionStore struct {
	database *gorm.DB
}

// NewActionStore 构造 mk_actions 表访问层。
func NewActionStore(database *gorm.DB) *ActionStore { return &ActionStore{database: database} }

// EnsureSchema 建 mk_actions 表。幂等，可在进程启动时调用。
func (s *ActionStore) EnsureSchema() error {
	if err := s.database.Exec(ensureActionsDDL).Error; err != nil {
		return fmt.Errorf("ensure mk_actions table: %w", err)
	}
	return nil
}

// CreateAction 落库一个对外动作（id 由数据库生成，CreatedAt 为空则取当前时间）。
// idem_key 重复 → 返回 IsDuplicateKeyError 可识别的错误（不覆盖已有动作）。
func (s *ActionStore) CreateAction(ctx context.Context, a *Action) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.IdemKey) == "" {
		return fmt.Errorf("marketing: action idem_key 不能为空（uk_idem 幂等键）")
	}
	if a.Status == "" {
		a.Status = ActionStatusDraft
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	return s.database.WithContext(ctx).Create(a).Error
}

// UpdateActionStatus 按主键更新动作状态（不存在 → gorm.ErrRecordNotFound）。
// 注意：MySQL 默认 DSN 未开启 clientFoundRows，UPDATE 返回「实际变更行数」——
// 把 status 更新为与当前相同的值会返回 RowsAffected=0（幂等状态设置），
// 不能据此判定记录不存在；故 0 行时再 Count 确认，仅记录确实缺失才返回 ErrRecordNotFound。
func (s *ActionStore) UpdateActionStatus(ctx context.Context, id int64, status string) error {
	if !ValidActionStatus(status) {
		return fmt.Errorf("marketing: 非法 action status %q（合法值：draft | reviewing | approved | rejected | sent | failed）", status)
	}
	res := s.database.WithContext(ctx).
		Model(&Action{}).
		Where("id = ?", id).
		Update("status", status)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		var n int64
		if err := s.database.WithContext(ctx).Model(&Action{}).Where("id = ?", id).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			return gorm.ErrRecordNotFound
		}
	}
	return nil
}

// IsDuplicateKeyError 判断是否为唯一键冲突（MySQL 1062 / sqlite UNIQUE 约束）。
func IsDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "1062") ||
		strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(strings.ToLower(msg), "unique constraint")
}
