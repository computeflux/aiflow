package deploy

// asset_store.go：S5 软件资产画像（原 internal/asset，批次 G 并入 deploy 域）。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store 是 software_assets / deploy_plans 两表访问层。
// 直接取 *gorm.DB（不走 internal/db 的重 config：这些工具只需 MySQL）。
type Store struct {
	database *gorm.DB
}

// NewStore 构造 software_assets 表访问层。
func NewStore(database *gorm.DB) *Store { return &Store{database: database} }

// ensureTables 两表 DDL：与 apps/core/hacks/sql/software_assets.sql / deploy_plans.sql 保持一致。
var ensureTables = []string{
	`CREATE TABLE IF NOT EXISTS software_assets (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL UNIQUE,
  path VARCHAR(255) NOT NULL,
  kind VARCHAR(32) NOT NULL,
  language VARCHAR(32),
  ports JSON,
  start_method VARCHAR(32) NOT NULL,
  service VARCHAR(128),
  interfaces JSON,
  owner VARCHAR(64) NOT NULL,
  profile JSON NOT NULL,
  auto_managed TINYINT(1) NOT NULL DEFAULT 0,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS deploy_plans (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL,
  plan JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'idle',
  last_run_at DATETIME,
  last_result JSON,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// EnsureSchema 建表。幂等，可在各进程启动时调用。
func (s *Store) EnsureSchema() error {
	for _, ddl := range ensureTables {
		if err := s.database.Exec(ddl).Error; err != nil {
			return fmt.Errorf("ensure asset table: %w", err)
		}
	}
	return nil
}

// EnsureOpsTables 保证 S5 工具留痕所需的 audit_logs / interventions 表存在
// （DDL 与 internal/approval.ensureTables 一致；工具进程不 import approval——
// 其重 config 依赖 Temporal/Qdrant/LLM，这些工具只需 MySQL）。
var opsTables = []string{
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
}

// EnsureOpsTables 建运维中心运行服务相关的表（幂等，进程启动时调用）。
func EnsureOpsTables(database *gorm.DB) error {
	for _, ddl := range opsTables {
		if err := database.Exec(ddl).Error; err != nil {
			return fmt.Errorf("ensure ops table: %w", err)
		}
	}
	return nil
}

// Upsert 按 name 插入或刷新资产画像。
func (s *Store) Upsert(ctx context.Context, a *SoftwareAsset) error {
	a.UpdatedAt = time.Now()
	return s.database.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"path", "kind", "language", "ports", "start_method", "service",
			"interfaces", "owner", "profile", "auto_managed", "updated_at",
		}),
	}).Create(a).Error
}

// UpsertAll 批量刷新（deployctl inventory 全量刷新用）。
func (s *Store) UpsertAll(ctx context.Context, assets []SoftwareAsset) error {
	for i := range assets {
		if err := s.Upsert(ctx, &assets[i]); err != nil {
			return fmt.Errorf("upsert %s: %w", assets[i].Name, err)
		}
	}
	return nil
}

// List 返回资产列表；autoManaged < 0 = 全部，0 = 仅不纳管，1 = 仅纳管。
func (s *Store) List(ctx context.Context, autoManaged int8) ([]SoftwareAsset, error) {
	q := s.database.WithContext(ctx).Order("name")
	switch autoManaged {
	case 0:
		q = q.Where("auto_managed = 0")
	case 1:
		q = q.Where("auto_managed = 1")
	}
	var items []SoftwareAsset
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Get 按 name 取资产。
func (s *Store) Get(ctx context.Context, name string) (*SoftwareAsset, error) {
	var a SoftwareAsset
	if err := s.database.WithContext(ctx).Where("name = ?", name).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// ManagedComposeServices 返回纳管资产中的 compose 服务名集合
// （deploy all 的健康检查 / health-patrol 巡检遍历用）。
func (s *Store) ManagedComposeServices(ctx context.Context) (map[string]bool, error) {
	items, err := s.List(ctx, 1)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, a := range items {
		if a.StartMethod == "compose" && a.Service != "" {
			out[a.Service] = true
		}
	}
	return out, nil
}

// SavePlan 按 name 插入或刷新部署计划。
func (s *Store) SavePlan(ctx context.Context, p *DeployPlan) error {
	return s.database.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{"plan", "status"}),
	}).Create(p).Error
}

// Plan 按 name 取部署计划。
func (s *Store) Plan(ctx context.Context, name string) (*DeployPlan, error) {
	var p DeployPlan
	if err := s.database.WithContext(ctx).Where("name = ?", name).First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// RecordPlanRun 写计划执行结果（status + last_run_at + last_result）。
func (s *Store) RecordPlanRun(ctx context.Context, name, status string, result any) error {
	now := time.Now()
	var resultJSON string
	if result != nil {
		b, err := json.Marshal(result)
		if err == nil {
			resultJSON = string(b)
		}
	}
	res := s.database.WithContext(ctx).Model(&DeployPlan{}).
		Where("name = ?", name).
		Updates(map[string]any{
			"status":      status,
			"last_run_at": now,
			"last_result": resultJSON,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("deploy plan %q not found", name)
	}
	return nil
}
