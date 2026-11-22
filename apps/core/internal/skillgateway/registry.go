package skillgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// 注册表常见错误。
var (
	ErrSkillNotFound   = errors.New("skill not found")
	ErrVersionNotFound = errors.New("skill version not found")
	ErrNoLiveVersion   = errors.New("skill has no live version")
)

// Registry 是 skills / skill_versions / skill_runs 三表的访问层。
type Registry struct {
	db *db.DB
}

// NewRegistry 创建注册表。
func NewRegistry(database *db.DB) *Registry { return &Registry{db: database} }

// ensureTables 三表 DDL：与 apps/core/hacks/sql/skills.sql 保持一致。
// skill_versions.config 是每个版本的完整 spec JSON 快照（P1 追加列）：
// 版本转 live 时用它更新 skills 行的运行配置，draft 版本不影响生产。
var ensureTables = []string{
	`CREATE TABLE IF NOT EXISTS skills (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL UNIQUE,
  owner VARCHAR(64) NOT NULL,
  runtime VARCHAR(16) NOT NULL,
  type VARCHAR(16) NOT NULL,
  entrypoint VARCHAR(512) NOT NULL DEFAULT '',
  addr VARCHAR(512) NOT NULL DEFAULT '',
  container VARCHAR(64) NOT NULL DEFAULT '',
  limits JSON NOT NULL,
  permissions JSON NULL,
  input_schema JSON NOT NULL,
  output_schema JSON NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS skill_versions (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  skill_id BIGINT NOT NULL,
  version VARCHAR(32) NOT NULL,
  image VARCHAR(255) NOT NULL,
  traffic_split INT DEFAULT 100,
  status VARCHAR(16) DEFAULT 'draft',
  rollback_to VARCHAR(32),
  config JSON NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_skill_version (skill_id, version),
  KEY idx_skill_versions_status (skill_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS skill_runs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  skill_name VARCHAR(128) NOT NULL,
  version VARCHAR(32) NOT NULL,
  trace_id VARCHAR(64) NOT NULL,
  workflow_id VARCHAR(64), run_id VARCHAR(64),
  input_hash CHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL,
  latency_ms INT, error TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  KEY idx_skill_time (skill_name, created_at),
  KEY idx_trace (trace_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// ensureColumns 兜底列（老表缺列时 ALTER ADD COLUMN，MySQL 8 无 IF NOT EXISTS）。
// ALTER 一律用 JSON NULL（对已有数据的表安全）；新表由上方 DDL 使用 JSON NOT NULL。
var ensureColumns = map[string]map[string]string{
	"skills": {
		"entrypoint":    "VARCHAR(512) NOT NULL DEFAULT ''",
		"addr":          "VARCHAR(512) NOT NULL DEFAULT ''",
		"container":     "VARCHAR(64) NOT NULL DEFAULT ''",
		"limits":        "JSON NULL",
		"permissions":   "JSON NULL",
		"input_schema":  "JSON NULL",
		"output_schema": "JSON NULL",
	},
	"skill_versions": {
		"config": "JSON NULL",
	},
}

// EnsureSchema 建表 + 补列。幂等，可在 API 每次启动时调用。
// 表/列名均来自本文件常量，不存在 SQL 注入面。
func (r *Registry) EnsureSchema() error {
	for _, ddl := range ensureTables {
		if err := r.db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("ensure skill table: %w", err)
		}
	}
	for table, defs := range ensureColumns {
		for col, def := range defs {
			var cnt int64
			if err := r.db.Raw(
				"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
				table, col,
			).Scan(&cnt).Error; err != nil {
				return fmt.Errorf("inspect column %s.%s: %w", table, col, err)
			}
			if cnt == 0 {
				alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def)
				if err := r.db.Exec(alter).Error; err != nil {
					return fmt.Errorf("alter table %s: %w", table, err)
				}
			}
		}
	}
	return nil
}

// Register 注册（或幂等刷新）一个 skill。
//
// 语义（docs/plan/transform-plan.md §6.5）：
//   - skill 不存在 → 建 skill 行 + 版本行（live, traffic=100）；
//   - skill 已存在且版本已存在 → 幂等：刷新该版本的 image 与 config 快照，
//     若该版本是 live 则同步更新 skills 行配置；
//   - skill 已存在且版本不存在 → 新版本行（draft, traffic=0），
//     不影响当前 live 配置。
//
// 返回 skill、版本以及 created（本次是否新建了行）。
func (r *Registry) Register(ctx context.Context, spec *SkillSpec) (*db.Skill, *db.SkillVersion, bool, error) {
	configJSON, err := specSnapshot(spec)
	if err != nil {
		return nil, nil, false, err
	}

	var skill db.Skill
	err = r.db.WithContext(ctx).Where("name = ?", spec.Name).First(&skill).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		skill = db.Skill{
			Name:         spec.Name,
			Owner:        spec.Owner,
			Runtime:      spec.Runtime,
			Type:         spec.Type,
			Entrypoint:   spec.Entrypoint,
			Addr:         spec.Addr,
			Container:    spec.Container,
			Limits:       mustJSONString(spec.Limits),
			Permissions:  nullableJSONString(spec.Permissions),
			InputSchema:  string(spec.InputSchema),
			OutputSchema: string(spec.OutputSchema),
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if err := r.db.WithContext(ctx).Create(&skill).Error; err != nil {
			return nil, nil, false, fmt.Errorf("create skill: %w", err)
		}
		version := db.SkillVersion{
			SkillID:      skill.ID,
			Version:      spec.Version,
			Image:        spec.Image,
			TrafficSplit: 100,
			Status:       "live",
			Config:       configJSON,
			CreatedAt:    time.Now(),
		}
		if err := r.db.WithContext(ctx).Create(&version).Error; err != nil {
			return nil, nil, false, fmt.Errorf("create skill version: %w", err)
		}
		return &skill, &version, true, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("query skill: %w", err)
	}

	var version db.SkillVersion
	err = r.db.WithContext(ctx).Where("skill_id = ? AND version = ?", skill.ID, spec.Version).First(&version).Error
	if err == nil {
		// 版本已存在：幂等刷新 image 与 config 快照；live 版本同时刷新 skills 行。
		version.Image = spec.Image
		version.Config = configJSON
		if err := r.db.WithContext(ctx).Save(&version).Error; err != nil {
			return nil, nil, false, fmt.Errorf("refresh skill version: %w", err)
		}
		if version.Status == "live" {
			if err := r.applyConfigToSkill(ctx, skill.ID, spec); err != nil {
				return nil, nil, false, err
			}
		}
		return &skill, &version, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, false, fmt.Errorf("query skill version: %w", err)
	}

	// 新版本 → draft，不影响 live。
	version = db.SkillVersion{
		SkillID:      skill.ID,
		Version:      spec.Version,
		Image:        spec.Image,
		TrafficSplit: 0,
		Status:       "draft",
		Config:       configJSON,
		CreatedAt:    time.Now(),
	}
	if err := r.db.WithContext(ctx).Create(&version).Error; err != nil {
		return nil, nil, false, fmt.Errorf("create skill version: %w", err)
	}
	return &skill, &version, true, nil
}

// SetTraffic 设置版本流量：traffic>0 → live，=0 → draft。
// 版本转 live 时把该版本的 config 快照写入 skills 行（配置随版本切换）。
func (r *Registry) SetTraffic(ctx context.Context, skillID uint, version string, traffic int) (*db.SkillVersion, error) {
	if traffic < 0 {
		traffic = 0
	}
	if traffic > 100 {
		traffic = 100
	}
	var v db.SkillVersion
	err := r.db.WithContext(ctx).Where("skill_id = ? AND version = ?", skillID, version).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query skill version: %w", err)
	}

	v.TrafficSplit = traffic
	if traffic > 0 {
		v.Status = "live"
	} else {
		v.Status = "draft"
	}
	if err := r.db.WithContext(ctx).Save(&v).Error; err != nil {
		return nil, fmt.Errorf("update skill version: %w", err)
	}

	if v.Status == "live" && v.Config != "" {
		var spec SkillSpec
		if err := json.Unmarshal([]byte(v.Config), &spec); err == nil {
			if err := r.applyConfigToSkill(ctx, skillID, &spec); err != nil {
				return nil, err
			}
		}
	}
	return &v, nil
}

// ResolveLive 解析 skill 的可服务版本；多版本时按 traffic_split 加权随机。
// status 含 'live' 与 'gray'：灰度版本（S1 §3.4）带 5/50% 流量参与路由，
// 完成灰度后转 live、旧版本 retired（traffic=0 自动出局）。
func (r *Registry) ResolveLive(ctx context.Context, name string) (*db.Skill, *db.SkillVersion, error) {
	var skill db.Skill
	err := r.db.WithContext(ctx).Where("name = ?", name).First(&skill).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, ErrSkillNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("query skill: %w", err)
	}

	var versions []db.SkillVersion
	if err := r.db.WithContext(ctx).
		Where("skill_id = ? AND status IN ? AND traffic_split > 0", skill.ID, []string{"live", "gray"}).
		Order("version").
		Find(&versions).Error; err != nil {
		return nil, nil, fmt.Errorf("query live versions: %w", err)
	}
	if len(versions) == 0 {
		return nil, nil, ErrNoLiveVersion
	}
	if len(versions) == 1 {
		return &skill, &versions[0], nil
	}

	total := 0
	for _, v := range versions {
		total += v.TrafficSplit
	}
	n := rand.IntN(total)
	for i := range versions {
		n -= versions[i].TrafficSplit
		if n < 0 {
			return &skill, &versions[i], nil
		}
	}
	return &skill, &versions[len(versions)-1], nil
}

// ListSkills 返回全部 skill（按创建时间正序）。
func (r *Registry) ListSkills(ctx context.Context) ([]db.Skill, error) {
	var skills []db.Skill
	if err := r.db.WithContext(ctx).Order("id").Find(&skills).Error; err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	return skills, nil
}

// ListVersions 返回某 skill 的全部版本（新版本在前）。
func (r *Registry) ListVersions(ctx context.Context, skillID uint) ([]db.SkillVersion, error) {
	var versions []db.SkillVersion
	if err := r.db.WithContext(ctx).Where("skill_id = ?", skillID).Order("id DESC").Find(&versions).Error; err != nil {
		return nil, fmt.Errorf("list skill versions: %w", err)
	}
	return versions, nil
}

// RecentRuns 返回某 skill 最近的执行记录。
func (r *Registry) RecentRuns(ctx context.Context, name string, limit int) ([]db.SkillRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var runs []db.SkillRun
	if err := r.db.WithContext(ctx).Where("skill_name = ?", name).Order("id DESC").Limit(limit).Find(&runs).Error; err != nil {
		return nil, fmt.Errorf("list skill runs: %w", err)
	}
	return runs, nil
}

// RecordRun 写入一条执行记录。
func (r *Registry) RecordRun(ctx context.Context, run *db.SkillRun) error {
	if err := r.db.WithContext(ctx).Create(run).Error; err != nil {
		return fmt.Errorf("record skill run: %w", err)
	}
	return nil
}

// applyConfigToSkill 用 spec 更新 skills 行的运行配置（owner/runtime/type/
// entrypoint/addr/container/limits/permissions/schema）。
func (r *Registry) applyConfigToSkill(ctx context.Context, skillID uint, spec *SkillSpec) error {
	updates := map[string]any{
		"owner":         spec.Owner,
		"runtime":       spec.Runtime,
		"type":          spec.Type,
		"entrypoint":    spec.Entrypoint,
		"addr":          spec.Addr,
		"container":     spec.Container,
		"limits":        mustJSONString(spec.Limits),
		"permissions":   nullableJSONString(spec.Permissions),
		"input_schema":  string(spec.InputSchema),
		"output_schema": string(spec.OutputSchema),
		"updated_at":    time.Now(),
	}
	if err := r.db.WithContext(ctx).Model(&db.Skill{}).Where("id = ?", skillID).Updates(updates).Error; err != nil {
		return fmt.Errorf("apply skill config: %w", err)
	}
	return nil
}

// VersionConfigAddr 从版本 config 快照取 addr（S5 §3.4 灰度路由：
// 版本实例容器经 deployctl version up 写入 config.addr，网关按版本 addr 路由；
// 快照无 addr（历史版本）时返回空，调用方回退 skills 行 addr）。
func VersionConfigAddr(configJSON string) string {
	if configJSON == "" || configJSON == "null" {
		return ""
	}
	var spec SkillSpec
	if err := json.Unmarshal([]byte(configJSON), &spec); err != nil {
		return ""
	}
	return spec.Addr
}

// specSnapshot 序列化完整 spec 作为版本快照。
func specSnapshot(spec *SkillSpec) (string, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal skill spec: %w", err)
	}
	return string(b), nil
}

// mustJSONString 序列化为 JSON 字符串；失败返回 "{}"（limits 必须非 NULL）。
func mustJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// nullableJSONString 序列化可空 JSON；空 RawMessage 返回 "null"。
func nullableJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}
