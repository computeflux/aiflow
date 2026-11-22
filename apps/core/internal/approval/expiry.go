package approval

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
)

// TickExpiry 扫 pending 且已过期的审批单 → expired + 升级通知（§3.2）。
// 不存在"默认通过"：超时一律失效，绝不自动放行。
func (s *Store) TickExpiry(ctx context.Context) (int, error) {
	now := time.Now()
	var rows []db.Approval
	if err := s.db.WithContext(ctx).
		Where("status = ? AND expires_at IS NOT NULL AND expires_at < ?", "pending", now).
		Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("list expired approvals: %w", err)
	}
	for i := range rows {
		a := &rows[i]
		a.Status = "expired"
		a.DecidedAt = &now
		if err := s.db.WithContext(ctx).Save(a).Error; err != nil {
			return i, fmt.Errorf("expire approval %s: %w", a.ApprovalNo, err)
		}
		s.Log(ctx, "expiry", "approval_expired", a.ApprovalNo, map[string]any{
			"kind": a.Kind, "subject": a.Subject, "proposer": a.Proposer,
		})
		// S9 §7.7：审批过期 → 关联进化单同步 expired（否则 task 永久停在 approving，
		// 工作项回流 "expired" 分支永不触发 → 工作项卡死 in_review，24h 后同一发现又开重复单）
		if res := s.db.WithContext(ctx).Model(&db.EvolveTask{}).
			Where("approval_id = ? AND status = ?", a.ID, "approving").
			Update("status", "expired"); res.Error != nil {
			log.Printf("WARN: expire evolve task for approval %s: %v", a.ApprovalNo, res.Error)
		} else if res.RowsAffected > 0 {
			s.Log(ctx, "expiry", "evolve_task_expired", a.ApprovalNo, map[string]any{
				"kind": a.Kind, "subject": a.Subject,
			})
		}
		var expiresAt time.Time
		if a.ExpiresAt != nil {
			expiresAt = *a.ExpiresAt
		}
		_ = s.notify.SendApproval(ctx, notify.ApprovalNotice{
			ID: a.ID, ApprovalNo: a.ApprovalNo, Kind: a.Kind, Subject: a.Subject,
			Proposer: a.Proposer, ExpiresAt: expiresAt, Expired: true,
		})
	}
	return len(rows), nil
}
