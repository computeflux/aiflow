package activities

import (
	"context"
	"errors"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// SaveRawItemsInput is the payload for persisting fetched raw items.
type SaveRawItemsInput struct {
	Items []db.RawItem `json:"items"`
}

// SaveRawItemsResult reports how many items were inserted / skipped and
// returns the actually-inserted items so the caller can index only new data.
type SaveRawItemsResult struct {
	Inserted int          `json:"inserted"`
	Skipped  int          `json:"skipped"`
	Items    []db.RawItem `json:"items"` // only newly inserted rows
}

// SaveRawItems persists fetched raw items to the database.
//
// Items whose (source, external_id) combination already exists in the
// database are silently skipped so that repeated fetches never produce
// duplicate rows.  Items with an empty external_id are also skipped
// because we cannot reliably deduplicate them.
func (a *Activities) SaveRawItems(ctx context.Context, input SaveRawItemsInput) (SaveRawItemsResult, error) {
	result := SaveRawItemsResult{}
	if len(input.Items) == 0 {
		return result, nil
	}

	now := time.Now()

	for i := range input.Items {
		item := &input.Items[i]

		// Require a non-empty external_id so we can deduplicate.
		if item.ExternalID == "" {
			result.Skipped++
			continue
		}

		// Check whether a row with the same (source, external_id)
		// already exists.
		var existing db.RawItem
		err := a.DB.Where("source = ? AND external_id = ?",
			item.Source, item.ExternalID).First(&existing).Error

		if err == nil {
			// Row already exists – skip.
			result.Skipped++
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return result, err // real database error
		}

		// New row – insert.
		if item.FetchedAt.IsZero() {
			item.FetchedAt = now
		}
		if err := a.DB.Create(item).Error; err != nil {
			return result, err
		}
		result.Inserted++
		result.Items = append(result.Items, *item)
	}

	return result, nil
}
