package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// routes_agents.go：Agent 注册/绑定路由（从 routes.go 拆出，纯机械搬迁）。

// agentManifest 是 agentctl register / board Agents 页提交的 manifest 字段子集。
type agentManifest struct {
	Name          string          `json:"name" binding:"required"`
	Role          string          `json:"role" binding:"required"`
	Version       string          `json:"version"`
	Entrypoint    string          `json:"entrypoint" binding:"required"`
	Container     string          `json:"container" binding:"required"`
	WorkdirPrefix string          `json:"workdir_prefix"`
	Limits        json.RawMessage `json:"limits"`
	Permissions   json.RawMessage `json:"permissions"`
	Config        json.RawMessage `json:"config"`
}

func (s *Store) listAgents(ctx *gin.Context) {
	var agents []db.AgentRegistry
	if err := s.db.WithContext(ctx).Order("name").Find(&agents).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var bindings []db.AgentBinding
	_ = s.db.WithContext(ctx).Find(&bindings).Error
	bindingMap := map[string]gin.H{}
	for _, b := range bindings {
		bindingMap[b.Role] = gin.H{
			"agent_name": b.AgentName, "version": b.Version,
			"updated_by": b.UpdatedBy, "updated_at": b.UpdatedAt,
		}
	}
	out := make([]gin.H, 0, len(agents))
	for _, a := range agents {
		out = append(out, gin.H{
			"name": a.Name, "role": a.Role, "version": a.Version,
			"entrypoint": a.Entrypoint, "container": a.Container,
			"workdir_prefix": a.WorkdirPrefix,
			"model_profile":  AgentModelProfile(a.Config),
		})
	}
	ctx.JSON(http.StatusOK, gin.H{"agents": out, "bindings": bindingMap})
}

func (s *Store) registerAgent(ctx *gin.Context) {
	var m agentManifest
	if err := ctx.ShouldBindJSON(&m); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// role：coder | reviewer | analyst（analyst = 需求分析/拆卡，复用 pi-web，只读工具）
	if m.Role != "coder" && m.Role != "reviewer" && m.Role != "analyst" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "role must be coder|reviewer|analyst"})
		return
	}
	if m.Version == "" {
		m.Version = "latest"
	}
	if m.WorkdirPrefix == "" {
		m.WorkdirPrefix = "/work"
	}
	a := db.AgentRegistry{
		Name: m.Name, Role: m.Role, Version: m.Version,
		Entrypoint: m.Entrypoint, Container: m.Container, WorkdirPrefix: m.WorkdirPrefix,
		Limits: string(m.Limits), Permissions: string(m.Permissions),
	}
	// config 里的 model_profile 由专用端点维护（S8.2）；agentctl register 传空 config
	// 时不得冲掉已设置的模型绑定。
	if len(m.Config) == 0 || string(m.Config) == "null" {
		var existing db.AgentRegistry
		if err := s.db.WithContext(ctx).Where("name = ?", m.Name).First(&existing).Error; err == nil {
			a.Config = existing.Config
		}
	} else {
		a.Config = string(m.Config)
	}
	if err := s.upsertAgent(ctx, &a); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.Log(ctx, "agentctl", "agent_registered", m.Name, gin.H{"role": m.Role, "version": m.Version, "container": m.Container})
	ctx.JSON(http.StatusOK, gin.H{"ok": true, "agent": gin.H{
		"name": a.Name, "role": a.Role, "version": a.Version, "container": a.Container,
	}})
}

type bindBody struct {
	AgentName string `json:"agent_name" binding:"required"`
	UpdatedBy string `json:"updated_by"`
}

// agentModelBody 设置/清除某 agent（按 name）的运行时模型档案（S8.2）。
// model_profile 空 = 清除绑定（回退 env / active 档案）。key 不单独存：执行时
// 从 model_profiles 表取（api_key 红线：agents_registry.config 绝不存密钥）。
type agentModelBody struct {
	ModelProfile string `json:"model_profile"`
}

// getAgentModel 返回 agent 当前绑定的模型档案名（agents_registry.config.model_profile）。
func (s *Store) getAgentModel(ctx *gin.Context) {
	name := ctx.Param("name")
	var a db.AgentRegistry
	if err := s.db.WithContext(ctx).Where("name = ?", name).First(&a).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "agent not registered: " + name})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"agent": a.Name, "model_profile": AgentModelProfile(a.Config)})
}

// setAgentModel 把指定模型档案绑到 agent 的运行时模型（pi-web coder = provider/modelId，
// ocr reviewer = provider/model）。档案必须存在且纳入 agent 目录（sync_agents=1，
// 保证运行时框架的模型目录里真的有它）。
func (s *Store) setAgentModel(ctx *gin.Context) {
	name := ctx.Param("name")
	var a db.AgentRegistry
	if err := s.db.WithContext(ctx).Where("name = ?", name).First(&a).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "agent not registered: " + name})
		return
	}
	var body agentModelBody
	if err := ctx.ShouldBindJSON(&body); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	profileName := strings.TrimSpace(body.ModelProfile)
	if profileName != "" {
		var p db.ModelProfile
		if err := s.db.WithContext(ctx).Where("name = ?", profileName).First(&p).Error; err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "model profile not found: " + profileName})
			return
		}
		if p.SyncAgents != 1 {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "档案未纳入 agent 目录（先勾选「agent 目录」保存）: " + profileName})
			return
		}
	}
	cfg := map[string]any{}
	if a.Config != "" {
		_ = json.Unmarshal([]byte(a.Config), &cfg) // 损坏 → 以空 config 重建
		if cfg == nil {
			cfg = map[string]any{}
		}
	}
	if profileName == "" {
		delete(cfg, "model_profile")
	} else {
		cfg["model_profile"] = profileName
	}
	b, _ := json.Marshal(cfg)
	a.Config = string(b)
	a.UpdatedAt = time.Now()
	if err := s.upsertAgent(ctx, &a); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.Log(ctx, actorOf(ctx), "agent_model_set", name, gin.H{"model_profile": profileName})
	ctx.JSON(http.StatusOK, gin.H{"ok": true, "agent": name, "model_profile": profileName})
}

// AgentModelProfile 从 agents_registry.config JSON 提取 model_profile（无/损坏 = ""）。
// 导出供 evolve-driver 解析 agent 级模型绑定（S8.2）。
func AgentModelProfile(config string) string {
	if strings.TrimSpace(config) == "" {
		return ""
	}
	var cfg struct {
		ModelProfile string `json:"model_profile"`
	}
	if json.Unmarshal([]byte(config), &cfg) != nil {
		return ""
	}
	return cfg.ModelProfile
}

func (s *Store) bindAgent(ctx *gin.Context) {
	role := ctx.Param("role")
	if role != "coder" && role != "reviewer" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "role must be coder|reviewer"})
		return
	}
	var body bindBody
	if err := ctx.ShouldBindJSON(&body); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var agent db.AgentRegistry
	if err := s.db.WithContext(ctx).Where("name = ?", body.AgentName).First(&agent).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "agent not registered: " + body.AgentName})
		return
	}
	if agent.Role != role {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("agent %s is %s, not %s", body.AgentName, agent.Role, role)})
		return
	}
	binding := db.AgentBinding{
		Role: role, AgentName: agent.Name, Version: agent.Version,
		UpdatedBy: body.UpdatedBy, UpdatedAt: time.Now(),
	}
	if err := s.upsertBinding(ctx, &binding); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.Log(ctx, body.UpdatedBy, "agent_bound", role, gin.H{"agent": agent.Name, "version": agent.Version})
	ctx.JSON(http.StatusOK, gin.H{"ok": true, "role": role, "agent": agent.Name})
}

// upsertAgent 按 name upsert（agentctl register 幂等刷新）。
func (s *Store) upsertAgent(ctx context.Context, a *db.AgentRegistry) error {
	var existing db.AgentRegistry
	err := s.db.WithContext(ctx).Where("name = ?", a.Name).First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		return s.db.WithContext(ctx).Create(a).Error
	}
	if err != nil {
		return err
	}
	a.CreatedAt = existing.CreatedAt
	return s.db.WithContext(ctx).Save(a).Error
}

// upsertBinding 按 role upsert（agentctl bind 运行时切换）。
func (s *Store) upsertBinding(ctx context.Context, b *db.AgentBinding) error {
	var existing db.AgentBinding
	err := s.db.WithContext(ctx).Where("role = ?", b.Role).First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		return s.db.WithContext(ctx).Create(b).Error
	}
	if err != nil {
		return err
	}
	b.ID = existing.ID
	return s.db.WithContext(ctx).Save(b).Error
}
