// Package activities 是 Temporal Go Worker 的业务活动集（抓取入库/检索评估/生成发布/点赞分发/邮件/工作流编排活动）。
package activities

import (
	"context"
	"fmt"

	"github.com/wetee/ai-workflow/internal/db"
)

// GetAccountInput selects an active account for a platform.
type GetAccountInput struct {
	Platform string `json:"platform"`
}

// GetAccountResult returns the account identifier.
type GetAccountResult struct {
	AccountID string `json:"account_id"`
}

// GetAccount returns the first active account for the requested platform.
func (a *Activities) GetAccount(ctx context.Context, input GetAccountInput) (GetAccountResult, error) {
	var account db.Account
	if err := a.DB.Where("platform = ? AND active = ?", input.Platform, true).First(&account).Error; err != nil {
		return GetAccountResult{}, fmt.Errorf("find active account: %w", err)
	}
	return GetAccountResult{AccountID: account.AccountID}, nil
}
