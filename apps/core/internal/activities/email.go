package activities

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/wetee/ai-workflow/internal/db"
	"go.temporal.io/sdk/activity"
	"gorm.io/gorm"
)

// EmailAnalysisInput 是邮件 AI 分析 activity 的输入：按组织名聚合素材。
type EmailAnalysisInput struct {
	OrgName string `json:"org_name"`
}

// EmailRecipientItem 是收件人素材项：邮箱、名称、仓库与推荐理由。
type EmailRecipientItem struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

// EmailAnalysisOutput 是邮件 AI 分析 activity 的输出：批次 id 与收件人数。
type EmailAnalysisOutput struct {
	BatchID uint `json:"batch_id"`
	Total   int  `json:"total"`
}

// FinalizeEmailBatchInput 是邮件批次收尾 activity 的输入。
type FinalizeEmailBatchInput struct {
	BatchID  uint   `json:"batch_id"`
	Status   string `json:"status"`
	Total    int    `json:"total"`
	ErrorMsg string `json:"error_msg"`
}

func (a *Activities) FinalizeEmailBatch(_ context.Context, input FinalizeEmailBatchInput) error {
	if input.BatchID == 0 {
		return nil
	}
	u := map[string]any{"status": input.Status}
	if input.ErrorMsg != "" {
		u["error_msg"] = input.ErrorMsg
	}
	if input.Total > 0 {
		u["total"] = input.Total
	}
	return a.DB.Model(&db.EmailBatch{}).Where("id = ?", input.BatchID).Updates(u).Error
}

func (a *Activities) RunEmailAnalysis(ctx context.Context, input EmailAnalysisInput) (EmailAnalysisOutput, error) {
	log.Printf("[email] starting analysis for org=%s", input.OrgName)

	wfID := activity.GetInfo(ctx).WorkflowExecution.ID
	batch := db.EmailBatch{OrgName: input.OrgName, Status: "analyzing", WorkflowID: wfID}
	if err := a.DB.Create(&batch).Error; err != nil {
		return EmailAnalysisOutput{}, fmt.Errorf("create batch: %w", err)
	}
	log.Printf("[email] batch #%d created, workflow=%s", batch.ID, wfID)

	// 全量加载可能拖垮内存，限制只处理最近一批（按 id 倒序取最新 5000 条）
	var items []db.RawItem
	if err := a.DB.Where("source = ? AND keyword = ?", db.PlatformGitHub, input.OrgName).Order("id desc").Limit(5000).Find(&items).Error; err != nil {
		a.markFailed(batch.ID, err.Error())
		return EmailAnalysisOutput{}, fmt.Errorf("read raw_items: %w", err)
	}
	log.Printf("[email] fetched %d raw_items", len(items))

	// Group by email (dedup same person across repos/commits).
	type author struct {
		email, name string
		repos       []string
		count       int
	}
	seen := make(map[string]*author)
	for _, item := range items {
		email := extractEmail(item.Content)
		if email == "" || !isValidEmail(email) {
			continue
		}
		name := ""
		content := item.Content
		if idx := strings.Index(content, "author: "); idx >= 0 {
			rest := content[idx+8:]
			if end := strings.Index(rest, " <"); end >= 0 {
				name = rest[:end]
			}
		}
		repo := item.ExternalID
		if idx := strings.Index(repo, "|"); idx >= 0 {
			repo = repo[:idx]
		}
		if a, ok := seen[email]; ok {
			a.count++
			if !contains(a.repos, repo) {
				a.repos = append(a.repos, repo)
			}
		} else {
			seen[email] = &author{email: email, name: name, repos: []string{repo}, count: 1}
		}
	}
	log.Printf("[email] %d unique valid emails from %d items", len(seen), len(items))

	var recipients []EmailRecipientItem
	for _, a := range seen {
		repo := a.repos[0]
		if len(a.repos) > 1 {
			repo = strings.Join(a.repos, ", ")
		}
		reason := fmt.Sprintf("%s 项目贡献者，%d 次提交", repo, a.count)
		recipients = append(recipients, EmailRecipientItem{
			Email:  a.email,
			Name:   a.name,
			Repo:   repo,
			Reason: reason,
		})
	}
	log.Printf("[email] %d recipients after programmatic filtering", len(recipients))

	// 批次收件人重建（更新批次 → 删除旧收件人 → 写入新收件人）放在同一事务，
	// 避免中途失败留下半新半旧的收件人列表。
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&batch).Updates(map[string]any{"status": db.StatusPending, "total": len(recipients)}).Error; err != nil {
			return fmt.Errorf("update batch: %w", err)
		}
		if err := tx.Where("batch_id = ?", batch.ID).Delete(&db.EmailRecipient{}).Error; err != nil {
			return fmt.Errorf("delete recipients: %w", err)
		}
		for _, r := range recipients {
			if err := tx.Create(&db.EmailRecipient{BatchID: batch.ID, Email: r.Email, Name: r.Name, Repo: r.Repo, Reason: r.Reason, Status: db.StatusPending}).Error; err != nil {
				return fmt.Errorf("create recipient: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		a.markFailed(batch.ID, err.Error())
		return EmailAnalysisOutput{}, err
	}
	log.Printf("[email] batch #%d done: %d recipients", batch.ID, len(recipients))
	return EmailAnalysisOutput{BatchID: batch.ID, Total: len(recipients)}, nil
}

func (a *Activities) markFailed(batchID uint, errMsg string) {
	a.DB.Model(&db.EmailBatch{}).Where("id = ?", batchID).Updates(map[string]any{"status": "failed", "error_msg": errMsg})
}

var emailRe = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

func isValidEmail(email string) bool {
	email = strings.ToLower(email)
	// Obvious non-human emails.
	for _, p := range []string{
		"noreply", "no-reply", "donotreply",
		"bot", "github-actions", "github-action",
		"users.noreply.github.com",
		"actions@", "action@",
	} {
		if strings.Contains(email, p) {
			return false
		}
	}
	return emailRe.MatchString(email)
}

func extractEmail(content string) string {
	if idx := strings.Index(content, "<"); idx >= 0 {
		if end := strings.Index(content[idx:], ">"); end >= 0 {
			return content[idx+1 : idx+end]
		}
	}
	return ""
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
