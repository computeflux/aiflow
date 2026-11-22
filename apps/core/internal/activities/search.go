package activities

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/qdrant"
)

// SearchInput is the payload for the vector search activity.
type SearchInput struct {
	Query string `json:"query"`
	K     int    `json:"k"`
	Days  int    `json:"days"`
}

// SearchResult returns search results.
type SearchResult struct {
	Results []qdrant.SearchResult `json:"results"`
}

// Search retrieves the most relevant chunks from Qdrant.
func (a *Activities) Search(ctx context.Context, input SearchInput) (SearchResult, error) {
	// 嵌入模型不可用（EMBEDDING_MODEL 未配置）时向量检索整体停用
	if !a.LLM.EmbeddingsEnabled() {
		return SearchResult{}, errors.New("knowledge search disabled: EMBEDDING_MODEL not configured")
	}

	vector, err := a.LLM.Embed(ctx, input.Query)
	if err != nil {
		return SearchResult{}, fmt.Errorf("embed query: %w", err)
	}

	var since *time.Time
	if input.Days > 0 {
		t := time.Now().AddDate(0, 0, -input.Days)
		since = &t
	}

	limit := uint64(input.K)
	if limit == 0 {
		limit = 5
	}

	results, err := a.Qdrant.Search(ctx, vector, limit, since)
	if err != nil {
		return SearchResult{}, fmt.Errorf("qdrant search: %w", err)
	}
	return SearchResult{Results: results}, nil
}
