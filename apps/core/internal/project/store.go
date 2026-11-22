package project

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Store 是 projects 表访问层（与 deploy.Store 同风格：自带 DDL，幂等）。
type Store struct {
	database *gorm.DB
}

// NewStore 构造 projects 表访问层（S9 §7.8 项目注册）。
func NewStore(database *gorm.DB) *Store { return &Store{database: database} }

// ensureDDL 建表 DDL（幂等，进程启动时调用）。
var ensureDDL = `CREATE TABLE IF NOT EXISTS projects (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL UNIQUE,
  dir VARCHAR(255) NOT NULL,
  runtime VARCHAR(32) NOT NULL,
  image VARCHAR(255) DEFAULT '',
  test_cmd VARCHAR(512) DEFAULT '',
  description VARCHAR(512) DEFAULT '',
  active TINYINT(1) DEFAULT 1,
  registered_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

// EnsureSchema 建表（幂等）。
func (s *Store) EnsureSchema() error {
	return s.database.Exec(ensureDDL).Error
}

// List 返回全部项目（按名称序）。
func (s *Store) List(ctx context.Context) ([]Project, error) {
	var out []Project
	if err := s.database.WithContext(ctx).Order("name ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ByName 按项目名（= 工作项 target_name）查询；不存在返回 nil, nil。
func (s *Store) ByName(ctx context.Context, name string) (*Project, error) {
	var p Project
	if err := s.database.WithContext(ctx).Where("name = ?", name).First(&p).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// Get 按 ID 查询。
func (s *Store) Get(ctx context.Context, id uint) (*Project, error) {
	var p Project
	if err := s.database.WithContext(ctx).First(&p, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// ScanResult 是一轮扫描注册的统计。
type ScanResult struct {
	Scanned int `json:"scanned"`
	Created int `json:"created"`
	Updated int `json:"updated"`
}

// ScanAndRegister 扫描仓库 apps/ 目录并注册（幂等）：
//   - 新目录 → 建项目（含默认 runtime/镜像/描述；board*/core* 默认停用）；
//   - 已有项目 → 刷新 dir/runtime/描述（**不覆盖**用户的 image/test_cmd/active 编辑）。
func (s *Store) ScanAndRegister(ctx context.Context, root string) (*ScanResult, error) {
	found, err := ScanApps(root)
	if err != nil {
		return nil, fmt.Errorf("scan apps: %w", err)
	}
	res := &ScanResult{Scanned: len(found)}
	for _, p := range found {
		var existing Project
		err := s.database.WithContext(ctx).Where("name = ?", p.Name).First(&existing).Error
		switch {
		case err == gorm.ErrRecordNotFound:
			if cerr := s.database.WithContext(ctx).Create(&p).Error; cerr != nil {
				return nil, fmt.Errorf("register %s: %w", p.Name, cerr)
			}
			res.Created++
		case err != nil:
			return nil, fmt.Errorf("lookup %s: %w", p.Name, err)
		default:
			updates := map[string]any{"dir": p.Dir, "runtime": p.Runtime, "updated_at": time.Now()}
			if existing.Description == "" && p.Description != "" {
				updates["description"] = p.Description
			}
			if uerr := s.database.WithContext(ctx).Model(&Project{}).Where("id = ?", existing.ID).Updates(updates).Error; uerr != nil {
				return nil, fmt.Errorf("refresh %s: %w", p.Name, uerr)
			}
			res.Updated++
		}
	}
	return res, nil
}

// Update 更新项目可编辑字段（image / test_cmd / description / active）。
func (s *Store) Update(ctx context.Context, id uint, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	fields["updated_at"] = time.Now()
	return s.database.WithContext(ctx).Model(&Project{}).Where("id = ?", id).Updates(fields).Error
}

// Delete 删除项目注册（不影响目录本身；已引用的工作项保持原 target_name）。
func (s *Store) Delete(ctx context.Context, id uint) error {
	return s.database.WithContext(ctx).Delete(&Project{}, id).Error
}
