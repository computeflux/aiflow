package activities

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/qdrant"
)

// IndexInput is the payload for the indexing activity.
type IndexInput struct {
	Items []db.RawItem `json:"items"`
}

// IndexResult returns the number of chunks indexed.
type IndexResult struct {
	Chunks int `json:"chunks"`
}

// Index chunks and embeds raw items, then upserts them into Qdrant.
//
// Each chunk receives a deterministic ID derived from its content so that
// repeated upserts are truly idempotent – the same content always maps to
// the same Qdrant point.
func (a *Activities) Index(ctx context.Context, input IndexInput) (IndexResult, error) {
	if len(input.Items) == 0 {
		return IndexResult{}, nil
	}

	const maxChunkSize = 1000
	const overlap = 200
	const embedBatch = 20 // smaller batches avoid API timeouts

	var chunks []qdrant.Chunk
	var contents []string

	for _, item := range input.Items {
		if item.Content == "" {
			continue
		}
		for _, c := range chunk(item.Content, maxChunkSize, overlap) {
			// Deterministic ID from source + content so upsert is idempotent.
			h := sha256.Sum256([]byte(item.Source + "|" + c))
			chunkID := fmt.Sprintf("%x", h[:16])

			chunks = append(chunks, qdrant.Chunk{
				ID:          chunkID,
				Content:     c,
				Source:      item.Source,
				URL:         item.URL,
				PublishedAt: item.PublishedAt,
			})
			contents = append(contents, c)
		}
	}

	if len(chunks) == 0 {
		return IndexResult{}, nil
	}

	// 嵌入模型不可用时跳过向量索引（EMBEDDING_MODEL 未配置）
	if !a.LLM.EmbeddingsEnabled() {
		return IndexResult{}, nil
	}

	vectors := make([][]float32, 0, len(contents))
	for i := 0; i < len(contents); i += embedBatch {
		end := i + embedBatch
		if end > len(contents) {
			end = len(contents)
		}
		batch, err := a.LLM.EmbedBatch(ctx, contents[i:end])
		if err != nil {
			return IndexResult{}, fmt.Errorf("embed batch: %w", err)
		}
		vectors = append(vectors, batch...)
		time.Sleep(50 * time.Millisecond) // simple rate limiter
	}

	if err := a.Qdrant.UpsertChunks(ctx, chunks, vectors); err != nil {
		return IndexResult{}, fmt.Errorf("upsert chunks: %w", err)
	}

	return IndexResult{Chunks: len(chunks)}, nil
}
