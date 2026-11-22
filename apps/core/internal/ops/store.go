// Package ops 是 S6 运维情报（docs/plan/S6-运维情报.md）：
// 聚合 error_logs / skill_runs / skill_circuit / interventions / Temporal →
// 每日 08:00 出 Markdown 日报、每周一 08:00 出升级建议（LLM 归纳，降级模式不出 LLM）→
// 落 ops_reports 表（type=daily|weekly|upgrade_suggestion，(report_date,type) 幂等 upsert）。
// 只读现有数据 + 只写 ops_reports，不新增采集、不触碰任何运行链路（§6 回滚：停 ops-report 即止）。
package ops

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store 是 ops_reports 表访问层。直接取 *gorm.DB（不走 internal/db 的重 config：
// 本服务只需 MySQL + 可选 LLM + 可选 Temporal）。
type Store struct {
	database *gorm.DB
}

// NewStore 构造 ops_reports 表访问层。
func NewStore(database *gorm.DB) *Store { return &Store{database: database} }

// OpsReport 是运维日志 / 升级建议（对应表 ops_reports，S6 §3.1/§3.2）。
// type：daily | weekly | upgrade_suggestion；content 为 Markdown；metrics 为聚合原始数据（JSON 字符串）。
type OpsReport struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	ReportDate time.Time `gorm:"type:date" json:"report_date"`
	Type       string    `gorm:"size:32" json:"type"`
	Content    string    `gorm:"type:longtext" json:"content"`
	Metrics    string    `gorm:"type:json" json:"metrics"`
	CreatedAt  time.Time `json:"created_at"`
}

func (OpsReport) TableName() string { return "ops_reports" }

// BeforeSave 把空的 metrics JSON 列归一为合法 JSON：MySQL 的 JSON 列**不接受空串**
// （空串会踩 Error 3140 "The document is empty"，见 internal/db/models.go 同类注释）。
func (r *OpsReport) BeforeSave(*gorm.DB) error {
	if r.Metrics == "" {
		r.Metrics = "{}"
	}
	return nil
}

// ensureTable DDL：与 apps/core/hacks/sql/ops_reports.sql 保持一致。
var ensureTable = `CREATE TABLE IF NOT EXISTS ops_reports (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  report_date DATE NOT NULL,
  type VARCHAR(32) NOT NULL,
  content LONGTEXT NOT NULL,
  metrics JSON NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_date_type (report_date, type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

// EnsureSchema 建表。幂等，可在进程启动时调用。
func (s *Store) EnsureSchema() error {
	if err := s.database.Exec(ensureTable).Error; err != nil {
		return fmt.Errorf("ensure ops_reports table: %w", err)
	}
	return nil
}

// Upsert 按 (report_date, type) 插入或刷新（重启不产生重复行）。
func (s *Store) Upsert(ctx context.Context, r *OpsReport) error {
	r.ReportDate = time.Date(r.ReportDate.Year(), r.ReportDate.Month(), r.ReportDate.Day(), 0, 0, 0, 0, time.Local)
	return s.database.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "report_date"}, {Name: "type"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"content", "metrics",
		}),
	}).Create(r).Error
}

// List 返回报告列表；type 为空 = 全部。按日期倒序。
func (s *Store) List(ctx context.Context, reportType string) ([]OpsReport, error) {
	q := s.database.WithContext(ctx).Order("report_date desc, id desc")
	if reportType != "" {
		q = q.Where("type = ?", reportType)
	}
	var items []OpsReport
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Get 按 id 取报告。
func (s *Store) Get(ctx context.Context, id uint) (*OpsReport, error) {
	var r OpsReport
	if err := s.database.WithContext(ctx).First(&r, id).Error; err != nil {
		return nil, err
	}
	return &r, nil
}
