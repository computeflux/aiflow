// Package intervention 是干预舱数据与 API（S3，docs/plan/S3-干预舱.md §3.3）：
// interventions 表（S1 提前建表）+ 六类干预动作路由（takeover/inject/retry/
// rollback/resolve/list）+ 活跃 workflow / 熔断状态查询（board 干预舱页用）。
// 所有干预动作写 audit_logs 留痕（actor=操作人，只增不删）。
package intervention

import (
	"context"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// Store 是 interventions 表访问层。
type Store struct {
	database *db.DB
}

// NewStore 构造干预记录（interventions 表）访问层。
func NewStore(database *db.DB) *Store { return &Store{database: database} }

// List 按状态过滤返回干预记录（status 为空 = 全部），新记录在前。
func (s *Store) List(ctx context.Context, status string) ([]db.Intervention, error) {
	q := s.database.WithContext(ctx).Order("id DESC").Limit(200)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var items []db.Intervention
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Get 按 id 取干预记录。
func (s *Store) Get(ctx context.Context, id uint) (*db.Intervention, error) {
	var item db.Intervention
	if err := s.database.WithContext(ctx).First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// Resolve 标记 resolved（附 note 追加到记录）。
func (s *Store) Resolve(ctx context.Context, id uint, note string) (*db.Intervention, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	updates := map[string]any{"status": "resolved", "resolved_at": now}
	if note != "" {
		if item.Note != "" {
			note = item.Note + "\n" + note
		}
		updates["note"] = note
	}
	if err := s.database.WithContext(ctx).Model(item).Updates(updates).Error; err != nil {
		return nil, err
	}
	item.Status = "resolved"
	item.ResolvedAt = &now
	return item, nil
}
