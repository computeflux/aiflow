// Package main is the generate-skill binary.
//
// Command generate-skill 是 generate 能力（LLM 生成中文社媒推文）的
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/llm"
)

// defaultAddr 是 generate-skill 的默认监听地址，与 skill.json 的 addr 一致。
const defaultAddr = ":9501"

func main() {
	cfg := config.Load()
	// 复用 workflow-api 同款 llm.Client，避免额外配置 LLM 凭据。
	client := llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.EmbeddingModel, cfg.LLMModel)

	addr := defaultAddr
	if a := os.Getenv("GENERATE_SKILL_ADDR"); a != "" {
		addr = a
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in GenerateSkillInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSkillError(w, http.StatusBadRequest, "invalid input: "+err.Error())
			return
		}
		out, err := GenerateRun(r.Context(), client, in)
		if err != nil {
			// 非 200 让 skillgateway 的 invokeService 直接转成 InvokeError。
			writeSkillError(w, http.StatusBadGateway, err.Error())
			return
		}
		// sources 为 nil 时序列化为 null，会过不了 skillgateway 的 output schema
		//（array 类型）。这里归一化为空数组，保证 {ok, sources:[]} 结构稳定。
		sources := out.Sources
		if sources == nil {
			sources = []string{}
		}
		writeSkillOK(w, map[string]any{
			"content": out.Content,
			"sources": sources,
		})
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	log.Printf("generate-skill listening on %s (LLM model=%s)", addr, cfg.LLMModel)
	log.Fatal(srv.ListenAndServe())
}

// writeSkillOK 写入成功响应：注入 ok=true 后编码。
// skillgateway 对 200 响应会做 output schema 校验 + ok 字段检查，故 ok 必带。
func writeSkillOK(w http.ResponseWriter, data map[string]any) {
	data["ok"] = true
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

// writeSkillError 写入错误响应：ok=false + reason，并返回非 200 状态码。
func writeSkillError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "reason": reason})
}
