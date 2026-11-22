// Package qdrant 是 Qdrant 向量库客户端（collection/upsert/过滤检索/scroll，REST+gRPC）。
package qdrant

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
)

// Chunk is a normalized piece of content stored in Qdrant.
type Chunk struct {
	ID          string     `json:"id"`
	Content     string     `json:"content"`
	Source      string     `json:"source"`
	Keyword     string     `json:"keyword"`
	URL         string     `json:"url"`
	PublishedAt *time.Time `json:"published_at"`
	// S7 产品情报：内部文档 metadata（分层授权 + 溯源）；旧数据为空。
	SourceType      string     `json:"source_type,omitempty"`     // product_doc | tech_doc | meeting_minutes | ticket | external
	Confidentiality string     `json:"confidentiality,omitempty"` // public | internal | secret
	DocTitle        string     `json:"doc_title,omitempty"`       // 来源文档标题（溯源展示）
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`      // 文档更新时间（溯源时间戳）
}

// SearchResult is a retrieved chunk with score.
type SearchResult struct {
	Chunk Chunk   `json:"chunk"`
	Score float32 `json:"score"`
}

// Client wraps the Qdrant gRPC client.
type Client struct {
	collection string
	vectorSize uint64
	inner      *qdrant.Client
}

// NewClient creates a Qdrant client.
func NewClient(host string, grpcPort int, collection string) (*Client, error) {
	client, err := qdrant.NewClient(
		&qdrant.Config{
			Host: host,
			Port: grpcPort,
		})
	if err != nil {
		return nil, fmt.Errorf("create qdrant client: %w", err)
	}
	return &Client{
		collection: collection,
		vectorSize: 1024,
		inner:      client,
	}, nil
}

// EnsureCollection creates the collection if it does not exist.
func (c *Client) EnsureCollection(ctx context.Context) error {
	exists, err := c.inner.CollectionExists(ctx, c.collection)
	if err != nil {
		return fmt.Errorf("check collection: %w", err)
	}
	if exists {
		return nil
	}
	return c.inner.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: c.collection,
		VectorsConfig: qdrant.NewVectorsConfig(
			&qdrant.VectorParams{
				Size:     c.vectorSize,
				Distance: qdrant.Distance_Cosine,
				OnDisk:   qdrant.PtrOf(false),
			}),
	})
}

// UpsertChunks inserts or updates chunks with their embedding vectors.
func (c *Client) UpsertChunks(ctx context.Context, chunks []Chunk, vectors [][]float32) error {
	if len(chunks) != len(vectors) {
		return fmt.Errorf("chunks and vectors length mismatch")
	}

	points := make([]*qdrant.PointStruct, len(chunks))
	for i, ch := range chunks {
		id := ch.ID
		if id == "" {
			id = uuid.NewString()
		}
		payload := map[string]any{
			"content": ch.Content,
			"source":  ch.Source,
			"keyword": ch.Keyword,
			"url":     ch.URL,
		}
		if ch.PublishedAt != nil {
			payload["published_at"] = float64(ch.PublishedAt.Unix())
		}
		// S7：内部文档 metadata（非空才写入，避免旧数据 payload 出现 null）
		if ch.SourceType != "" {
			payload["source_type"] = ch.SourceType
		}
		if ch.Confidentiality != "" {
			payload["confidentiality"] = ch.Confidentiality
		}
		if ch.DocTitle != "" {
			payload["doc_title"] = ch.DocTitle
		}
		if ch.UpdatedAt != nil {
			payload["updated_at"] = float64(ch.UpdatedAt.Unix())
		}
		points[i] = &qdrant.PointStruct{
			Id:      qdrant.NewIDUUID(id),
			Vectors: qdrant.NewVectors(vectors[i]...),
			Payload: qdrant.NewValueMap(payload),
		}
	}

	_, err := c.inner.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: c.collection,
		Points:         points,
	})
	return err
}

// Search retrieves the top-k chunks matching the embedding vector.
func (c *Client) Search(ctx context.Context, vector []float32, limit uint64, since *time.Time) ([]SearchResult, error) {
	var filter *qdrant.Filter
	if since != nil {
		gte := float64(since.Unix())
		filter = &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewRange("published_at", &qdrant.Range{Gte: &gte}),
			},
		}
	}

	resp, err := c.inner.Query(ctx, &qdrant.QueryPoints{
		CollectionName: c.collection,
		Query:          qdrant.NewQuery(vector...),
		Limit:          qdrant.PtrOf(limit),
		Filter:         filter,
		WithPayload:    qdrant.NewWithPayload(true),
		WithVectors:    qdrant.NewWithVectors(false),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant query: %w", err)
	}

	results := make([]SearchResult, 0, len(resp))
	for _, r := range resp {
		content := ""
		if v, ok := r.Payload["content"]; ok && v != nil {
			content = v.GetStringValue()
		}
		source := ""
		if v, ok := r.Payload["source"]; ok && v != nil {
			source = v.GetStringValue()
		}
		keyword := ""
		if v, ok := r.Payload["keyword"]; ok && v != nil {
			keyword = v.GetStringValue()
		}
		url := ""
		if v, ok := r.Payload["url"]; ok && v != nil {
			url = v.GetStringValue()
		}
		var publishedAt *time.Time
		if v, ok := r.Payload["published_at"]; ok && v != nil {
			if ts := v.GetDoubleValue(); ts > 0 {
				t := time.Unix(int64(ts), 0)
				publishedAt = &t
			}
		}
		// S7：读回内部文档 metadata（旧数据无这些键 → 空值）
		sourceType := payloadStr(r.Payload, "source_type")
		confidentiality := payloadStr(r.Payload, "confidentiality")
		docTitle := payloadStr(r.Payload, "doc_title")
		var updatedAt *time.Time
		if v, ok := r.Payload["updated_at"]; ok && v != nil {
			if ts := v.GetDoubleValue(); ts > 0 {
				t := time.Unix(int64(ts), 0)
				updatedAt = &t
			}
		}
		results = append(results, SearchResult{
			Score: r.Score,
			Chunk: Chunk{
				ID:              r.GetId().GetUuid(),
				Content:         content,
				Source:          source,
				Keyword:         keyword,
				URL:             url,
				PublishedAt:     publishedAt,
				SourceType:      sourceType,
				Confidentiality: confidentiality,
				DocTitle:        docTitle,
				UpdatedAt:       updatedAt,
			},
		})
	}
	return results, nil
}

// payloadStr 从 Qdrant payload 取字符串值（键不存在/非字符串 → 空串）。
func payloadStr(m map[string]*qdrant.Value, key string) string {
	if v, ok := m[key]; ok && v != nil {
		return v.GetStringValue()
	}
	return ""
}

// SearchFiltered 按向量检索并叠加 payload 过滤：
//   - since 非空 → published_at >= since；
//   - confidentialityLevels 非空 → confidentiality 属于该集合（S7 分层授权：
//     普通用户只传 [public]，管理员 [public, internal]，白名单角色 [public, internal, secret]）。
//
// 两者为 AND 关系。confidentiality 为空的旧数据（外部抓取）在传 levels 时被过滤掉。
func (c *Client) SearchFiltered(ctx context.Context, vector []float32, limit uint64, since *time.Time, confidentialityLevels []string) ([]SearchResult, error) {
	must := make([]*qdrant.Condition, 0, 2)
	if since != nil {
		gte := float64(since.Unix())
		must = append(must, qdrant.NewRange("published_at", &qdrant.Range{Gte: &gte}))
	}
	if len(confidentialityLevels) > 0 {
		must = append(must, qdrant.NewMatchKeywords("confidentiality", confidentialityLevels...))
	}
	var filter *qdrant.Filter
	if len(must) > 0 {
		filter = &qdrant.Filter{Must: must}
	}

	resp, err := c.inner.Query(ctx, &qdrant.QueryPoints{
		CollectionName: c.collection,
		Query:          qdrant.NewQuery(vector...),
		Limit:          qdrant.PtrOf(limit),
		Filter:         filter,
		WithPayload:    qdrant.NewWithPayload(true),
		WithVectors:    qdrant.NewWithVectors(false),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant filtered query: %w", err)
	}
	return parseSearchResp(resp), nil
}

// parseSearchResp 把 Qdrant 查询响应解析为 SearchResult 列表（Search 与 SearchFiltered 共用）。
func parseSearchResp(resp []*qdrant.ScoredPoint) []SearchResult {
	results := make([]SearchResult, 0, len(resp))
	for _, r := range resp {
		content := ""
		if v, ok := r.Payload["content"]; ok && v != nil {
			content = v.GetStringValue()
		}
		source := payloadStr(r.Payload, "source")
		keyword := payloadStr(r.Payload, "keyword")
		url := payloadStr(r.Payload, "url")
		var publishedAt *time.Time
		if v, ok := r.Payload["published_at"]; ok && v != nil {
			if ts := v.GetDoubleValue(); ts > 0 {
				t := time.Unix(int64(ts), 0)
				publishedAt = &t
			}
		}
		var updatedAt *time.Time
		if v, ok := r.Payload["updated_at"]; ok && v != nil {
			if ts := v.GetDoubleValue(); ts > 0 {
				t := time.Unix(int64(ts), 0)
				updatedAt = &t
			}
		}
		results = append(results, SearchResult{
			Score: r.Score,
			Chunk: Chunk{
				ID:              r.GetId().GetUuid(),
				Content:         content,
				Source:          source,
				Keyword:         keyword,
				URL:             url,
				PublishedAt:     publishedAt,
				SourceType:      payloadStr(r.Payload, "source_type"),
				Confidentiality: payloadStr(r.Payload, "confidentiality"),
				DocTitle:        payloadStr(r.Payload, "doc_title"),
				UpdatedAt:       updatedAt,
			},
		})
	}
	return results
}

// Scroll retrieves points linearly from the collection for background inspection.
func (c *Client) Scroll(ctx context.Context, limit uint32) ([]*qdrant.RetrievedPoint, error) {
	resp, err := c.inner.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: c.collection,
		Limit:          qdrant.PtrOf(limit),
		WithPayload:    qdrant.NewWithPayload(true),
		WithVectors:    qdrant.NewWithVectors(false),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant scroll: %w", err)
	}
	return resp, nil
}
