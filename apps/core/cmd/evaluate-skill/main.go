// Package main is the evaluate-skill binary.
//
// Command evaluate-skill 是 evaluate 能力（CRAG 相关性评估）的
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/llm"
)

// defaultAddr 是 evaluate-skill 的默认监听地址，与 skill.json 一致。
const defaultAddr = ":9502"

func main() {
	cfg := config.Load()
	// 复用 workflow-api 同款 llm.Client。
	client := llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.EmbeddingModel, cfg.LLMModel)

	addr := defaultAddr
	if a := os.Getenv("EVALUATE_SKILL_ADDR"); a != "" {
		addr = a
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in EvaluateSkillInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSkillError(w, http.StatusBadRequest, "invalid input: "+err.Error())
			return
		}
		out, err := EvaluateRun(r.Context(), client, in)
		if err != nil {
			writeSkillError(w, http.StatusBadGateway, err.Error())
			return
		}
		// evaluations 为 nil 时会序列化为 null，过不了 output schema（array）。
		// 归一化为空数组，保证 {ok, evaluations:[], sufficient} 结构稳定。
		evals := out.Evaluations
		if evals == nil {
			evals = []EvaluateSkillEvaluation{}
		}
		writeSkillOK(w, map[string]any{
			"evaluations": evals,
			"sufficient":  out.Sufficient,
		})
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	log.Printf("evaluate-skill listening on %s (LLM model=%s)", addr, cfg.LLMModel)
	log.Fatal(srv.ListenAndServe())
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
