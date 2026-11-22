// Package main is the search-skill binary.
//
// Command search-skill 是 knowledge search（向量检索）的 Skill Gateway service 端点。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/qdrant"
)

// defaultAddr 是 search-skill 的默认监听地址，与 skill.json 一致。
const defaultAddr = ":9503"

// searchInput 对应 internal/activities.SearchInput。
type searchInput struct {
	Query string `json:"query"`
	K     int    `json:"k"`
	Days  int    `json:"days"`
}

// searchResult 对应 internal/activities.SearchResult。
type searchResult struct {
	Results []qdrant.SearchResult `json:"results"`
}

func main() {
	cfg := config.Load()
	client := llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.EmbeddingModel, cfg.LLMModel)
	qc, err := qdrant.NewClient(cfg.QdrantHost, cfg.QdrantGRPCPort, cfg.QdrantCollection)
	if err != nil {
		log.Fatalf("qdrant client: %v", err)
	}
	addr := defaultAddr
	if a := os.Getenv("SEARCH_SKILL_ADDR"); a != "" {
		addr = a
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in searchInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSkillError(w, http.StatusBadRequest, "invalid input: "+err.Error())
			return
		}
		out, err := search(r.Context(), client, qc, in)
		if err != nil {
			writeSkillError(w, http.StatusBadGateway, err.Error())
			return
		}
		// results 为 nil 时序列化为 null，过不了 output schema（array）。
		results := out.Results
		if results == nil {
			results = []qdrant.SearchResult{}
		}
		writeSkillOK(w, map[string]any{
			"results": results,
			"count":   len(results),
		})
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	log.Printf("search-skill listening on %s (collection=%s)", addr, cfg.QdrantCollection)
	log.Fatal(srv.ListenAndServe())
}

// search 复用 internal/activities.Search 的逻辑：嵌入查询向量 → Qdrant 向量检索。
func search(ctx context.Context, client *llm.Client, qc *qdrant.Client, in searchInput) (searchResult, error) {
	if !client.EmbeddingsEnabled() {
		return searchResult{}, errors.New("knowledge search disabled: EMBEDDING_MODEL not configured")
	}
	vector, err := client.Embed(ctx, in.Query)
	if err != nil {
		return searchResult{}, fmt.Errorf("embed query: %w", err)
	}
	var since *time.Time
	if in.Days > 0 {
		t := time.Now().AddDate(0, 0, -in.Days)
		since = &t
	}
	limit := uint64(in.K)
	if limit == 0 {
		limit = 5
	}
	results, err := qc.Search(ctx, vector, limit, since)
	if err != nil {
		return searchResult{}, fmt.Errorf("qdrant search: %w", err)
	}
	return searchResult{Results: results}, nil
}

// writeSkillOK 注入 ok=true 后编码；skillgateway 对 200 响应做 output schema 校验。
func writeSkillOK(w http.ResponseWriter, data map[string]any) {
	data["ok"] = true
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

// writeSkillError 写入 ok=false + reason，并返回非 200 状态码。
func writeSkillError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "reason": reason})
}
