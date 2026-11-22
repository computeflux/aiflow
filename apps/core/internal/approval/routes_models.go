package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/db"
)

// routes_models.go：模型管理（S8）。模型档案（model_profiles 表）的 CRUD +
// 应用（active 切换）+ 同步到 ai-flow（model_list + 热 reload）。
//
// 应用范围（consumers，逗号分隔；空 = 全部）：
//   - core    Go 核心 LLM（api/audit/ops-report/... 经 ActiveModelConfig 周期热切）
//   - aiflow   ai-flow 网关（apply 时推 model_list + /reload）
//   - coder   evolve-driver coder agent（PI_MODEL，下次进化执行生效）
//   - reviewer evolve-driver reviewer agent（OCR_MODEL，同上）
//
// S8.2 agent 级模型（/agents/:name/model，见 routes_agents.go）：board「Agent 管理」页可给
// 每个 agent（按 agent 名，不限于 coder/reviewer 两角色）单独指定模型档案，优先级高于上面的
// 全局 consumers 匹配；evolve-driver 执行时解析为 provider="model-managed" + 档案 model。
// ocr 侧同步为 per-profile 子 provider（custom_providers["model-managed/<档案名>"]，各带
// url/api_key，多档案多 endpoint 时 key 不串）；pi-web 侧统一在 providers["model-managed"]。
//
// ⚠️ api_key 红线：列表/详情接口一律脱敏（只回 has_api_key）；写入不回显。

// RegisterModelRoutes 注册模型管理路由（前缀 /model-config，挂 requireAuth）。
func RegisterModelRoutes(r *gin.Engine, s *Store, mws ...gin.HandlerFunc) {
	g := r.Group("/model-config", mws...)
	g.GET("", s.listModels)
	g.POST("", s.createModel)
	g.PUT("/:id", s.updateModel)
	g.DELETE("/:id", s.deleteModel)
	g.POST("/:id/apply", s.applyModel)
	g.POST("/sync-aiflow", s.syncAIFlow)
}

// modelView 是脱敏后的档案投影（api_key → has_api_key）。
type modelView struct {
	ID             uint   `json:"id"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	BaseURL        string `json:"base_url"`
	HasAPIKey      bool   `json:"has_api_key"`
	EmbeddingModel string `json:"embedding_model"`
	Consumers      string `json:"consumers"`
	SyncAgents     *int   `json:"sync_agents"`
	Active         int    `json:"active"`
	Note           string `json:"note"`
	UpdatedBy      string `json:"updated_by"`
	UpdatedAt      string `json:"updated_at"`
}

func toModelView(p db.ModelProfile) modelView {
	return modelView{
		ID: p.ID, Name: p.Name, Provider: p.Provider, Model: p.Model,
		BaseURL: p.BaseURL, HasAPIKey: p.APIKey != "",
		EmbeddingModel: p.EmbeddingModel, Consumers: p.Consumers,
		SyncAgents: &p.SyncAgents,
		Active:     p.Active, Note: p.Note, UpdatedBy: p.UpdatedBy,
		UpdatedAt: p.UpdatedAt.Format(time.RFC3339),
	}
}

type modelBody struct {
	Name           string `json:"name" binding:"required"`
	Provider       string `json:"provider"`
	Model          string `json:"model" binding:"required"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	EmbeddingModel string `json:"embedding_model"`
	Consumers      string `json:"consumers"`
	Note           string `json:"note"`
	// SyncAgents：该档案是否纳入「同步给 agent 框架」的目录（S8.1）。
	// nil = 默认开启（建档即入 agent 目录）；false = 仅供全局热切，不进目录。
	SyncAgents *bool `json:"sync_agents"`
}

// syncAgentsEnabled 默认入 agent 目录（未显式关闭时）。
func (b *modelBody) syncAgentsEnabled() bool {
	return b.SyncAgents == nil || *b.SyncAgents
}

func (b *modelBody) apply(p *db.ModelProfile) {
	p.Name = b.Name
	p.Provider = b.Provider
	if p.Provider == "" {
		p.Provider = "openai"
	}
	p.Model = b.Model
	p.BaseURL = b.BaseURL
	p.EmbeddingModel = b.EmbeddingModel
	p.Consumers = b.Consumers
	p.Note = b.Note
	if b.APIKey != "" { // 空 = 保留原 key（编辑不回填）
		p.APIKey = b.APIKey
	}
}

func (s *Store) listModels(ctx *gin.Context) {
	var profiles []db.ModelProfile
	if err := s.db.WithContext(ctx).Order("active DESC, id").Find(&profiles).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]modelView, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, toModelView(p))
	}
	ctx.JSON(http.StatusOK, gin.H{"models": out})
}

func (s *Store) createModel(ctx *gin.Context) {
	var b modelBody
	if err := ctx.ShouldBindJSON(&b); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var cnt int64
	s.db.WithContext(ctx).Model(&db.ModelProfile{}).Where("name = ?", b.Name).Count(&cnt)
	if cnt > 0 {
		ctx.JSON(http.StatusConflict, gin.H{"error": "模型名已存在: " + b.Name})
		return
	}
	p := db.ModelProfile{}
	b.apply(&p)
	p.SyncAgents = 1
	if b.SyncAgents != nil && !*b.SyncAgents {
		p.SyncAgents = 0
	}
	p.UpdatedBy = actorOf(ctx)
	if err := s.db.WithContext(ctx).Create(&p).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.Log(ctx, actorOf(ctx), "model_created", p.Name, gin.H{"model": p.Model})
	ctx.JSON(http.StatusOK, gin.H{"ok": true, "model": toModelView(p)})
}

func (s *Store) updateModel(ctx *gin.Context) {
	id, ok := pathID(ctx)
	if !ok {
		return
	}
	var p db.ModelProfile
	if err := s.db.WithContext(ctx).First(&p, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	var b modelBody
	if err := ctx.ShouldBindJSON(&b); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var cnt int64
	s.db.WithContext(ctx).Model(&db.ModelProfile{}).Where("name = ? AND id <> ?", b.Name, id).Count(&cnt)
	if cnt > 0 {
		ctx.JSON(http.StatusConflict, gin.H{"error": "模型名已存在: " + b.Name})
		return
	}
	b.apply(&p)
	if b.SyncAgents != nil {
		p.SyncAgents = 1
		if !*b.SyncAgents {
			p.SyncAgents = 0
		}
	}
	p.UpdatedBy = actorOf(ctx)
	if err := s.db.WithContext(ctx).Save(&p).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.Log(ctx, actorOf(ctx), "model_updated", p.Name, gin.H{"model": p.Model})
	ctx.JSON(http.StatusOK, gin.H{"ok": true, "model": toModelView(p)})
}

func (s *Store) deleteModel(ctx *gin.Context) {
	id, ok := pathID(ctx)
	if !ok {
		return
	}
	var p db.ModelProfile
	if err := s.db.WithContext(ctx).First(&p, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	if p.Active == 1 {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "生效中的模型不能删除，请先应用其他模型"})
		return
	}
	if err := s.db.WithContext(ctx).Delete(&p).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.Log(ctx, actorOf(ctx), "model_deleted", p.Name, nil)
	ctx.JSON(http.StatusOK, gin.H{"ok": true})
}

// applyModel 把指定档案设为 active：单 active（应用层事务），随后尽力同步
// ai-flow（档案 consumers 含 aiflow 时）。Go 核心侧由周期 ticker 拉取生效。
func (s *Store) applyModel(ctx *gin.Context) {
	id, ok := pathID(ctx)
	if !ok {
		return
	}
	var p db.ModelProfile
	if err := s.db.WithContext(ctx).First(&p, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	tx := s.db.WithContext(ctx)
	if err := tx.Model(&db.ModelProfile{}).Where("id <> ?", p.ID).Update("active", 0).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := tx.Model(&db.ModelProfile{}).Where("id = ?", p.ID).Update("active", 1).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.Log(ctx, actorOf(ctx), "model_applied", p.Name, gin.H{"model": p.Model, "consumers": p.Consumers})

	res := gin.H{"ok": true, "applied": toModelView(p)}
	if consumerHas(p.Consumers, "aiflow") {
		if msg, err := s.pushAIBot(); err != nil {
			res["aiflow"] = "同步失败：" + err.Error()
		} else {
			res["aiflow"] = msg
		}
	}
	// S8.1：agent 框架模型目录同步（pi-web/ocr 宿主配置文件；ai-flow 走上方 /models）。
	if agents, err := s.SyncAgentsToFrameworks(); err != nil {
		res["agents"] = "同步失败：" + err.Error()
	} else if len(agents) > 0 {
		res["agents"] = agents
	}
	ctx.JSON(http.StatusOK, res)
}

// syncAIFlow 手动把 active 档案推送到 ai-flow（model_list + reload）。
func (s *Store) syncAIFlow(ctx *gin.Context) {
	msg, err := s.pushAIBot()
	if err != nil {
		ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"ok": true, "aiflow": msg})
}

// pushAIBot 把当前 active 档案写入 ai-flow 的 model_list 并触发热 reload。
// ai-flow 地址 = AI_FLOW_BASE_URL（默认 http://ai-flow:8134）。
func (s *Store) pushAIBot() (string, error) {
	var p db.ModelProfile
	if err := s.db.Where("active = 1").First(&p).Error; err != nil {
		return "", fmt.Errorf("no active model profile: %w", err)
	}
	base := os.Getenv("AI_FLOW_BASE_URL")
	if base == "" {
		base = "http://ai-flow:8134"
	}
	body := map[string]any{
		"models": []map[string]any{{
			"model_name": p.Name,
			"provider":   p.Provider,
			"model":      p.Model,
			"api_base":   p.BaseURL,
			"api_keys":   []string{p.APIKey},
			"enabled":    true,
		}},
		"active_model": p.Name,
	}
	b, _ := json.Marshal(body)
	code, respBody, err := httpPut(base+"/models", b)
	if err != nil {
		return "", fmt.Errorf("ai-flow /models: %w", err)
	}
	if code >= 300 {
		return "", fmt.Errorf("ai-flow /models: %d %s", code, truncate(string(respBody), 200))
	}
	return truncate(string(respBody), 200), nil
}

// listSyncAgentProfiles 取纳入 agent 目录的档案（sync_agents=1）。
func (s *Store) listSyncAgentProfiles() ([]db.ModelProfile, error) {
	var ps []db.ModelProfile
	if err := s.db.Where("sync_agents = 1").Order("active DESC, id").Find(&ps).Error; err != nil {
		return nil, err
	}
	return ps, nil
}

// SyncAgentsToFrameworks 把 agent 模型目录（sync_agents=1 档案）同步到 pi-web/ocr
// 宿主配置（ai-flow 由 pushAIBot 另走 /models）。active 档案名用于默认模型/enabledModels。
func (s *Store) SyncAgentsToFrameworks() (map[string]string, error) {
	profiles, err := s.listSyncAgentProfiles()
	if err != nil {
		return nil, err
	}
	hostHome := os.Getenv("CODER_HOST_HOME")
	if hostHome == "" {
		hostHome = "/host"
	}
	fallbackKey := os.Getenv("CODER_API_KEY")
	activeName := ""
	if p, err := s.ActiveModelConfig(); err == nil && p != nil {
		activeName = p.Name
	}
	return SyncAgentsCatalog(profiles, hostHome, activeName, fallbackKey), nil
}

// ActiveModelConfig 返回当前 active 档案（无 active 时 nil，调用方保持 env
// 配置）。供各 Go 服务周期热切 LLM。
func (s *Store) ActiveModelConfig() (*db.ModelProfile, error) {
	var p db.ModelProfile
	if err := s.db.Where("active = 1").First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// actorOf 取当前请求的操作者身份（JWT sub，authn 注入）；未登录 = "system"。
func actorOf(ctx *gin.Context) string {
	if uid := authn.UserID(ctx); uid != "" {
		return uid
	}
	return "system"
}

// pathID 解析 :id 路径参数；非法时写 400 并返回 ok=false。
func pathID(ctx *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(ctx.Param("id"), 10, 64)
	if err != nil || n == 0 {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return uint(n), true
}

// httpPut 向内部服务 PUT JSON（ai-flow /models 目录替换只接受 GET/PUT）。
func httpPut(url string, body []byte) (int, []byte, error) {
	return httpRequest(http.MethodPut, url, body)
}

func httpRequest(method, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, nil
}

func consumerHas(consumers, name string) bool {
	if consumers == "" {
		return true // 空 = 全部消费面
	}
	for _, c := range splitTrim(consumers) {
		if c == name {
			return true
		}
	}
	return false
}

func splitTrim(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ',' || r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
