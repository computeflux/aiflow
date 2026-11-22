// Package crag 实现 CRAG 检索（检索→评估→重检索/生成的 corrective 闭环，Qdrant + LLM）。
package crag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/qdrant"
)

// Result returns a CRAG-style answer with sources and relevance.
type Result struct {
	Query      string                `json:"query"`
	Sufficient bool                  `json:"sufficient"`
	Reasoning  string                `json:"reasoning"`
	Documents  []qdrant.SearchResult `json:"documents"`
	Answer     string                `json:"answer"`
}

// CRAG performs retrieval with relevance evaluation.
type CRAG struct {
	Qdrant *qdrant.Client
	LLM    *llm.Client
}

// New creates a CRAG instance.
func New(qc *qdrant.Client, llmClient *llm.Client) *CRAG {
	return &CRAG{Qdrant: qc, LLM: llmClient}
}

type evalResult struct {
	Sufficient bool   `json:"sufficient"`
	Reasoning  string `json:"reasoning"`
}

// Search retrieves documents, evaluates relevance, and synthesizes an answer.
// 无密级过滤（外部抓取数据兼容入口）；S7 分层授权走 SearchFiltered。
func (c *CRAG) Search(ctx context.Context, query string, k int, days int) (*Result, error) {
	return c.SearchFiltered(ctx, query, k, days, nil)
}

// SearchFiltered 与 Search 一致，但按密级集合过滤（S7 分层授权 + secret 红线）：
// confidentialityLevels 非空 → 只检索该密级的 chunk；secret 级内容不送外部 LLM（脱敏）。
func (c *CRAG) SearchFiltered(ctx context.Context, query string, k int, days int, confidentialityLevels []string) (*Result, error) {
	// 嵌入模型不可用时（EMBEDDING_MODEL 未配置）暂时停用知识库搜索，
	// 返回说明性的空结果而非报错，避免 API 500。
	if !c.LLM.EmbeddingsEnabled() {
		return &Result{
			Query:      query,
			Sufficient: false,
			Reasoning:  "知识库搜索已暂时停用：嵌入模型不可用（EMBEDDING_MODEL 未配置）",
			Answer:     "知识库搜索已暂时停用：嵌入模型不可用（EMBEDDING_MODEL 未配置）。",
		}, nil
	}

	vector, err := c.LLM.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	var since *time.Time
	if days > 0 {
		t := time.Now().AddDate(0, 0, -days)
		since = &t
	}
	if k <= 0 {
		k = 5
	}

	docs, err := c.Qdrant.SearchFiltered(ctx, vector, uint64(k), since, confidentialityLevels)
	if err != nil {
		return nil, fmt.Errorf("qdrant search: %w", err)
	}
	if len(docs) == 0 {
		return &Result{
			Query:      query,
			Sufficient: false,
			Reasoning:  "no documents found",
			Documents:  docs,
			Answer:     "",
		}, nil
	}

	// Evaluate relevance（secret 级脱敏，不送外部 LLM）。
	var sb strings.Builder
	for i, d := range docs {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, llmText(d.Chunk)))
	}
	system := "You evaluate whether the retrieved documents can answer the query. Reply with ONLY a JSON object: {\"sufficient\": true/false, \"reasoning\": \"...\"}."
	user := fmt.Sprintf("Query: %s\n\nDocuments:\n%s", query, sb.String())

	evalStr, err := c.LLM.ChatCompletion(ctx, system, user)
	if err != nil {
		return nil, fmt.Errorf("evaluate: %w", err)
	}

	var eval evalResult
	sufficient := false
	reasoning := evalStr

	// Try JSON parse first; fall back to string match.
	if err := json.Unmarshal([]byte(extractJSON(evalStr)), &eval); err == nil {
		sufficient = eval.Sufficient
		reasoning = eval.Reasoning
	} else {
		sufficient = strings.Contains(strings.ToLower(evalStr), `"sufficient":true`)
	}

	// Always attempt to synthesize an answer on best-effort basis.
	answer := c.synthesize(ctx, query, docs)

	return &Result{
		Query:      query,
		Sufficient: sufficient,
		Reasoning:  reasoning,
		Documents:  docs,
		Answer:     answer,
	}, nil
}

func (c *CRAG) synthesize(ctx context.Context, query string, docs []qdrant.SearchResult) string {
	var sb strings.Builder
	for _, d := range docs {
		_, _ = sb.WriteString(llmText(d.Chunk) + "\n\n")
	}
	system := "You are a helpful assistant. Answer the query concisely in Chinese based only on the provided documents. If the documents are irrelevant, say '当前知识库中没有找到相关信息'."
	user := fmt.Sprintf("Query: %s\n\nDocuments:\n%s", query, sb.String())
	answer, err := c.LLM.ChatCompletion(ctx, system, user)
	if err != nil {
		return ""
	}
	return answer
}

// llmText 把 chunk 转成给外部 LLM 的文本；secret 级脱敏（红线：正文不送外部 provider）。
func llmText(ch qdrant.Chunk) string {
	if ch.Confidentiality == "secret" {
		return fmt.Sprintf("[secret 内容已脱敏，来源：%s]", ch.DocTitle)
	}
	return ch.Content
}

// extractJSON extracts the first JSON object from a string.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "{"); idx >= 0 {
		if end := strings.LastIndex(s, "}"); end > idx {
			return s[idx : end+1]
		}
	}
	return s
}
