// Package modeladmin exposes the model catalog management endpoints for the
// ai-bot gateway ([workflow-fork] S8 模型管理):
//
//	GET  /models   → catalog: the current model_list (secrets masked) + the
//	             active agents.defaults.model_name.
//	PUT  /models   → replace the catalog: persist model_list + active model
//	             into config.json and hot-reload (same path as /reload), so
//	             every agent of the loop switches to the new model set.
//
// The store is injected by the gateway runtime (it owns the config file
// path); the handler itself is a plain http.Handler mounted on the shared
// health/reload mux, so it shares the gateway's network posture (internal
// :8134 only; the board reaches it through the core-ui nginx proxy).
package modeladmin

import (
	"encoding/json"
	"net/http"

	"github.com/computeflux/ai-flow/pkg/config"
)

// Store supplies the current config snapshot and persists catalog changes.
type Store interface {
	// Catalog returns the current model list (entries never carry api_keys
	// in responses — callers get has_api_key only) and the active default
	// model name.
	Catalog() ([]config.ModelConfig, string, error)
	// Apply replaces the model catalog and the active default model,
	// persists config.json and hot-reloads all agents. It returns the new
	// active model name.
	Apply(models []config.ModelConfig, active string) (string, error)
}

// Handler serves GET/PUT /models.
type Handler struct {
	store Store
}

var _ http.Handler = (*Handler)(nil)

// NewHandler creates the /models handler.
func NewHandler(s Store) *Handler { return &Handler{store: s} }

// catalogResponse is the GET /models body.
type catalogResponse struct {
	Models       []modelView `json:"models"`
	Active       string      `json:"active_model"`
	ActiveExists bool        `json:"active_exists"`
}

// modelView is a secret-free projection of config.ModelConfig for the UI.
type modelView struct {
	ModelName string `json:"model_name"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	APIBase   string `json:"api_base,omitempty"`
	Enabled   bool   `json:"enabled"`
	HasAPIKey bool   `json:"has_api_key"`
}

func toView(m config.ModelConfig) modelView {
	return modelView{
		ModelName: m.ModelName,
		Provider:  m.Provider,
		Model:     m.Model,
		APIBase:   m.APIBase,
		Enabled:   m.Enabled,
		HasAPIKey: m.APIKey() != "",
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ServeHTTP implements GET/PUT /models.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.get(w)
	case http.MethodPut:
		h.put(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed, use GET or PUT"})
	}
}

func (h *Handler) get(w http.ResponseWriter) {
	models, active, err := h.store.Catalog()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := catalogResponse{Models: make([]modelView, 0, len(models)), Active: active}
	for _, m := range models {
		resp.Models = append(resp.Models, toView(m))
		if m.ModelName == active {
			resp.ActiveExists = true
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Models []config.ModelConfig `json:"models"`
		Active string               `json:"active_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body: " + err.Error()})
		return
	}
	if len(input.Models) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "models is required (non-empty)"})
		return
	}
	active, err := h.store.Apply(input.Models, input.Active)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "active_model": active, "models": len(input.Models)})
}
