package activities

import (
	"context"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// GetPendingLikesInput selects raw items that have not been liked yet.
type GetPendingLikesInput struct {
	Source  string `json:"source"`
	Keyword string `json:"keyword"`
	Limit   int    `json:"limit"`
}

// GetPendingLikesResult returns the candidate raw items to like.
type GetPendingLikesResult struct {
	Items []db.RawItem `json:"items"`
}

// GetPendingLikes fetches raw items that have a URL but haven't been liked yet.
func (a *Activities) GetPendingLikes(ctx context.Context, input GetPendingLikesInput) (GetPendingLikesResult, error) {
	source := input.Source
	if source == "" {
		source = db.PlatformX
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}

	q := a.DB.Where("source = ? AND liked = ? AND url <> ?", source, false, "")
	if input.Keyword != "" {
		q = q.Where("keyword = ?", input.Keyword)
	}

	var items []db.RawItem
	if err := q.Order("fetched_at DESC").Limit(limit).Find(&items).Error; err != nil {
		return GetPendingLikesResult{}, err
	}
	return GetPendingLikesResult{Items: items}, nil
}

// MarkRawItemsLikedInput marks the given raw items (by source + external_id) as liked.
type MarkRawItemsLikedInput struct {
	Source      string   `json:"source"`
	ExternalIDs []string `json:"external_ids"`
}

// MarkRawItemsLiked flags matching raw items as liked and timestamps them.
func (a *Activities) MarkRawItemsLiked(ctx context.Context, input MarkRawItemsLikedInput) error {
	if len(input.ExternalIDs) == 0 {
		return nil
	}
	source := input.Source
	if source == "" {
		source = db.PlatformX
	}
	now := time.Now()
	return a.DB.Model(&db.RawItem{}).
		Where("source = ? AND external_id IN ?", source, input.ExternalIDs).
		Updates(map[string]any{"liked": true, "liked_at": &now}).Error
}
