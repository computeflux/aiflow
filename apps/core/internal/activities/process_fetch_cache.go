package activities

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/qdrant"
	"gorm.io/gorm"
)

// ProcessCacheInput selects rows from the fetch_cache table.
type ProcessCacheInput struct {
	WorkflowID string `json:"workflow_id"`
	BatchSize  int    `json:"batch_size"`
}

// ProcessCacheResult summarises what was done.
type ProcessCacheResult struct {
	Saved   int `json:"saved"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	Chunks  int `json:"chunks"`
}

// ProcessFetchCache drains the fetch_cache table for a given workflow,
// persists new rows to raw_items, indexes them into Qdrant, and deletes
// processed cache rows.
func (a *Activities) ProcessFetchCache(ctx context.Context, input ProcessCacheInput) (ProcessCacheResult, error) {
	if input.WorkflowID == "" {
		return ProcessCacheResult{}, fmt.Errorf("missing workflow_id")
	}
	if input.BatchSize <= 0 {
		input.BatchSize = 1000
	}

	var result ProcessCacheResult
	seen := make(map[string]bool)

	for {
		var rows []db.FetchCache
		if err := a.DB.Where("workflow_id = ?", input.WorkflowID).
			Order("id").Limit(input.BatchSize).Find(&rows).Error; err != nil {
			return result, fmt.Errorf("read fetch_cache: %w", err)
		}
		if len(rows) == 0 {
			break
		}

		ids := make([]uint, 0, len(rows))
		newItems := make([]db.RawItem, 0, len(rows))
		newItemCacheIDs := make([]uint, 0, len(rows))

		for i := range rows {
			r := &rows[i]
			if r.ExternalID == "" {
				ids = append(ids, r.ID)
				result.Skipped++
				continue
			}

			dedupKey := r.Source + "|" + r.ExternalID
			if seen[dedupKey] {
				ids = append(ids, r.ID)
				result.Skipped++
				continue
			}

			var existing db.RawItem
			err := a.DB.Where("source = ? AND external_id = ?",
				r.Source, r.ExternalID).First(&existing).Error
			if err == nil {
				// Update stale fields with fresh data (engagement, content, etc.)
				if err := a.DB.Model(&existing).Where("id = ?", existing.ID).UpdateColumns(map[string]any{
					"content":    r.Content,
					"raw_json":   r.RawJSON,
					"fetched_at": r.FetchedAt,
				}).Error; err != nil {
					// 更新失败时保留 fetch_cache 行，下次运行重试
					return result, fmt.Errorf("update raw_items: %w", err)
				}
				ids = append(ids, r.ID)
				result.Updated++
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return result, fmt.Errorf("check raw_items: %w", err)
			}

			seen[dedupKey] = true

			item := db.RawItem{
				Source:      r.Source,
				ExternalID:  r.ExternalID,
				Keyword:     r.Keyword,
				URL:         r.URL,
				Content:     r.Content,
				PublishedAt: r.PublishedAt,
				FetchedAt:   r.FetchedAt,
				RawJSON:     r.RawJSON,
			}
			newItems = append(newItems, item)
			newItemCacheIDs = append(newItemCacheIDs, r.ID)
			ids = append(ids, r.ID)
		}

		// 逐行插入：单行撞 1062 唯一键时跳过该行（不视为失败），其余错误整体返回；
		// 插入与对应 fetch_cache 删除放在同一事务，只有真正落库的行才删缓存，
		// 避免多行 Create 撞唯一键时整批失败导致已处理数据丢失。
		if len(newItems) > 0 {
			inserted := make([]db.RawItem, 0, len(newItems))
			err := a.DB.Transaction(func(tx *gorm.DB) error {
				cacheIDsToDelete := make([]uint, 0, len(newItems))
				for i := range newItems {
					item := newItems[i]
					if err := tx.Create(&item).Error; err != nil {
						var me *mysql.MySQLError
						if errors.As(err, &me) && me.Number == 1062 {
							// 行已存在（如并发 workflow 已插入）——按跳过处理，保留缓存行
							result.Skipped++
							continue
						}
						return fmt.Errorf("insert raw_items: %w", err)
					}
					inserted = append(inserted, item)
					cacheIDsToDelete = append(cacheIDsToDelete, newItemCacheIDs[i])
					result.Saved++
				}
				if len(cacheIDsToDelete) > 0 {
					if err := tx.Where("id IN ?", cacheIDsToDelete).Delete(&db.FetchCache{}).Error; err != nil {
						return fmt.Errorf("delete fetch_cache: %w", err)
					}
				}
				return nil
			})
			if err != nil {
				return result, err
			}

			if len(inserted) > 0 {
				chunks, err := a.indexItems(ctx, inserted)
				if err != nil {
					return result, fmt.Errorf("index items: %w", err)
				}
				result.Chunks += chunks
			}
		}

		if len(ids) > 0 {
			if err := a.DB.Where("id IN ?", ids).Delete(&db.FetchCache{}).Error; err != nil {
				return result, fmt.Errorf("delete fetch_cache: %w", err)
			}
		}
	}

	// Update last_run_at for scheduled ingest tasks
	if id, ok := parseScheduledIngestID(input.WorkflowID); ok {
		now := time.Now()
		a.DB.Model(&db.IngestTask{}).Where("id = ?", id).Updates(map[string]any{
			"last_run_at": now,
			"last_status": "success",
		})
	}

	return result, nil
}

// parseScheduledIngestID extracts the task ID from "scraper-{source}-{id}".
func parseScheduledIngestID(wid string) (uint, bool) {
	parts := strings.Split(wid, "-")
	if len(parts) >= 2 {
		var id int
		if n, _ := fmt.Sscanf(parts[len(parts)-1], "%d", &id); n == 1 {
			return uint(id), true
		}
	}
	return 0, false
}

// indexItems chunks, embeds and upserts items.  Embedding calls run
// concurrently to reduce total latency.
func (a *Activities) indexItems(ctx context.Context, items []db.RawItem) (int, error) {
	const maxChunkSize = 1000
	const overlap = 200
	const embedBatch = 20
	const maxConcurrent = 15

	var chunks []qdrant.Chunk
	var contents []string

	for _, item := range items {
		if item.Content == "" {
			continue
		}
		for _, c := range chunk(item.Content, maxChunkSize, overlap) {
			h := sha256.Sum256([]byte(item.Source + "|" + c))
			chunks = append(chunks, qdrant.Chunk{
				ID:          fmt.Sprintf("%x", h[:16]),
				Content:     c,
				Source:      item.Source,
				Keyword:     item.Keyword,
				URL:         item.URL,
				PublishedAt: item.PublishedAt,
			})
			contents = append(contents, c)
		}
	}

	if len(chunks) == 0 {
		return 0, nil
	}

	// 嵌入模型不可用时跳过向量索引（EMBEDDING_MODEL 未配置），
	// 原始内容仍已落库到 raw_items，后续模型恢复后可再补索引。
	if !a.LLM.EmbeddingsEnabled() {
		log.Printf("embedding disabled: skipping vector index for %d chunks (EMBEDDING_MODEL not set)", len(chunks))
		return 0, nil
	}

	type span struct{ from, to int }
	batches := make([]span, 0)
	for i := 0; i < len(contents); i += embedBatch {
		end := i + embedBatch
		if end > len(contents) {
			end = len(contents)
		}
		batches = append(batches, span{i, end})
	}

	log.Printf("embedding %d chunks in %d batches (max %d concurrent)",
		len(contents), len(batches), maxConcurrent)

	results := make([][][]float32, len(batches))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	start := time.Now()

	for idx, b := range batches {
		wg.Add(1)
		go func(idx, from, to int) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			log.Printf("embed batch %d/%d starting (%d texts)", idx+1, len(batches), to-from)
			t0 := time.Now()

			batchVectors, err := a.LLM.EmbedBatch(ctx, contents[from:to])

			log.Printf("embed batch %d/%d done in %v (err=%v)", idx+1, len(batches), time.Since(t0), err)

			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()

			if err == nil {
				results[idx] = batchVectors
			}
		}(idx, b.from, b.to)
	}

	wg.Wait()
	log.Printf("all %d embed batches finished in %v", len(batches), time.Since(start))

	if firstErr != nil {
		return 0, fmt.Errorf("embed batch: %w", firstErr)
	}

	vectors := make([][]float32, 0, len(contents))
	for _, r := range results {
		vectors = append(vectors, r...)
	}

	if err := a.Qdrant.UpsertChunks(ctx, chunks, vectors); err != nil {
		return 0, fmt.Errorf("upsert chunks: %w", err)
	}

	return len(chunks), nil
}
