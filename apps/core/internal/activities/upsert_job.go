package activities

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wetee/ai-workflow/internal/db"
)

// UpsertJobInput stores a generated tweet as a pending publish job.
type UpsertJobInput struct {
	Platform  string   `json:"platform"`
	AccountID string   `json:"account_id"`
	Content   string   `json:"content"`
	Sources   []string `json:"sources"`
}

// UpsertJobResult returns the created job ID.
type UpsertJobResult struct {
	JobID uint `json:"job_id"`
}

// UpsertJob creates a publish job record in the database.
func (a *Activities) UpsertJob(ctx context.Context, input UpsertJobInput) (UpsertJobResult, error) {
	sourcesJSON, err := json.Marshal(input.Sources)
	if err != nil {
		return UpsertJobResult{}, fmt.Errorf("marshal sources: %w", err)
	}

	job := &db.PublishJob{
		Platform:  input.Platform,
		AccountID: input.AccountID,
		Content:   input.Content,
		Sources:   string(sourcesJSON),
		Status:    db.StatusPending,
	}
	if err := a.DB.Create(job).Error; err != nil {
		return UpsertJobResult{}, fmt.Errorf("create job: %w", err)
	}
	return UpsertJobResult{JobID: job.ID}, nil
}
