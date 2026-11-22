package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// GetJobInput selects a publish job by ID.
type GetJobInput struct {
	ID uint `json:"id"`
}

// GetJobResult returns the job status and content.
type GetJobResult struct {
	Job db.PublishJob `json:"job"`
}

// GetJob fetches a publish job from the database.
func (a *Activities) GetJob(ctx context.Context, input GetJobInput) (GetJobResult, error) {
	var job db.PublishJob
	if err := a.DB.First(&job, input.ID).Error; err != nil {
		return GetJobResult{}, fmt.Errorf("find job: %w", err)
	}
	return GetJobResult{Job: job}, nil
}

// UpdateJobStatusInput updates a job status.
type UpdateJobStatusInput struct {
	ID     uint   `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// UpdateJobStatus sets the status of a publish job and timestamps.
func (a *Activities) UpdateJobStatus(ctx context.Context, input UpdateJobStatusInput) error {
	updates := map[string]any{
		"status": input.Status,
	}
	if input.Error != "" {
		updates["error_msg"] = input.Error
	}
	switch input.Status {
	case db.StatusApproved:
		now := time.Now()
		updates["approved_at"] = &now
	case db.StatusPublished:
		now := time.Now()
		updates["published_at"] = &now
	}
	return a.DB.Model(&db.PublishJob{}).Where("id = ?", input.ID).Updates(updates).Error
}
