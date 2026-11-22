// Package marketing 是营销判断服务（docs/marketing/implementation-spec.md）：
// 所有 LLM 判断（J1–J18）集中到一处，prompt / 模型 / 校验只维护一份，
// 每条判断自动可回放（上下文包 + 模型原始输出落库）。
//
// ⚠️ 一卡一组文件（P2 串行红线）：每张卡只动自己的文件，避免合并冲突。
//
//	MK-10 → model.go（域模型 + 枚举校验）
//	MK-11 → store_judgment.go（mk_judgments 表 + store，本文件）
//	MK-19 → store_lead.go / store_action.go（mk_leads / mk_actions）
package marketing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Judgment 域模型（含 GORM 标签与 TableName）已收敛到 model.go（MK-10）；
// 本文件（MK-11）只保留 mk_judgments 表访问层（Store）+ ensureDDL（一卡一组文件，P2 串行红线）。

// ensureDDL 建表 DDL：与 apps/core/hacks/sql/marketing_judgments.sql 保持一致
// （幂等，进程启动时调用；双轨：SQL 文件给 hacks/bootstrap.sh，ensureDDL 给服务自启兜底）。
var ensureDDL = `CREATE TABLE IF NOT EXISTS mk_judgments (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  region VARCHAR(16) NOT NULL,
  product VARCHAR(16) NOT NULL DEFAULT 'network',
  kind VARCHAR(32) NOT NULL,
  source VARCHAR(32) NOT NULL,
  source_ref VARCHAR(255) DEFAULT '',
  model VARCHAR(64) DEFAULT '',
  prompt_version VARCHAR(32) DEFAULT '',
  context_hash CHAR(64) NOT NULL DEFAULT '',
  context_snapshot LONGTEXT,
  raw_output LONGTEXT,
  verdict VARCHAR(32) DEFAULT '',
  reason TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  KEY idx_context_hash (context_hash),
  KEY idx_region_kind_created (region, kind, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

// Store 是 mk_judgments 表访问层（直接取 *gorm.DB，与 workitem.Store / product.Store 同风格）。
type Store struct {
	database *gorm.DB
}

// NewStore 构造 mk_judgments 表访问层。
func NewStore(database *gorm.DB) *Store { return &Store{database: database} }

// EnsureSchema 建表。幂等，可在进程启动时调用。
func (s *Store) EnsureSchema() error {
	if err := s.database.Exec(ensureDDL).Error; err != nil {
		return fmt.Errorf("ensure mk_judgments table: %w", err)
	}
	return nil
}

// ---- CRUD ----

// Create 落库一条判断记录（id 由数据库生成，CreatedAt 为空则取当前时间）。
func (s *Store) Create(ctx context.Context, j *Judgment) error {
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	return s.database.WithContext(ctx).Create(j).Error
}

// ByID 按主键取判断记录（不存在 → gorm.ErrRecordNotFound）。
func (s *Store) ByID(ctx context.Context, id uint) (*Judgment, error) {
	var j Judgment
	if err := s.database.WithContext(ctx).Where("id = ?", id).First(&j).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

// ByContextHash 按上下文包 hash 回放（取最新一条；不存在 → (nil, nil)）。
func (s *Store) ByContextHash(ctx context.Context, hash string) (*Judgment, error) {
	var j Judgment
	err := s.database.WithContext(ctx).
		Where("context_hash = ?", hash).
		Order("id DESC").First(&j).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// JudgmentFilter 是 List 的过滤条件（空值 = 不过滤）。
type JudgmentFilter struct {
	Region  string // intl | cn
	Product string // network | shop
	Kind    string // judge_lead | ...
	Verdict string // pass | revise | reject | ...
	Limit   int    // 0 = 不限
}

// List 列出判断记录（created_at 倒序，最新在前）。
func (s *Store) List(ctx context.Context, f JudgmentFilter) ([]Judgment, error) {
	q := s.database.WithContext(ctx)
	if f.Region != "" {
		q = q.Where("region = ?", f.Region)
	}
	if f.Product != "" {
		q = q.Where("product = ?", f.Product)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Verdict != "" {
		q = q.Where("verdict = ?", f.Verdict)
	}
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	var items []Judgment
	if err := q.Order("created_at DESC, id DESC").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Count 统计判断记录数（过滤条件同 List，不含 Limit）。
// 必须显式 .Model(&Judgment{})：GORM 的 Count 在 Statement.Model == nil 时会把
// Dest（*int64）当 Model 去 Parse，非结构体直接报 "Table not set"（gorm v1.31.2）。
func (s *Store) Count(ctx context.Context, f JudgmentFilter) (int64, error) {
	q := s.database.WithContext(ctx).Model(&Judgment{})
	if f.Region != "" {
		q = q.Where("region = ?", f.Region)
	}
	if f.Product != "" {
		q = q.Where("product = ?", f.Product)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Verdict != "" {
		q = q.Where("verdict = ?", f.Verdict)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}
