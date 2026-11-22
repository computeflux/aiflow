package skillgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/circuitbreaker"
	"github.com/wetee/ai-workflow/internal/db"
)

// Gateway 是 skill 框架对外的总入口：注册表 + 调用 runner。
type Gateway struct {
	reg    *Registry
	runner *Runner
}

// New 创建 Gateway。
func New(database *db.DB) *Gateway {
	reg := NewRegistry(database)
	return &Gateway{reg: reg, runner: NewRunner(reg)}
}

// WithBreaker 给底层 runner 挂接熔断器（S3 §3.1）；onOpen 在 breaker 首次 open 时回调。
func (g *Gateway) WithBreaker(brk *circuitbreaker.Registry, onOpen func(name string)) *Gateway {
	g.runner.WithBreaker(brk, onOpen)
	return g
}

// EnsureSchema 建表 + 补列（API 启动时调用，幂等）。
func (g *Gateway) EnsureSchema() error { return g.reg.EnsureSchema() }

// RegisterRoutes 挂载 skill API 到 /api/skills 路由组。
func RegisterRoutes(rg *gin.RouterGroup, gw *Gateway) {
	rg.GET("", gw.listSkills)
	rg.GET("/:name", gw.getSkill)
	rg.GET("/:name/runs", gw.listRuns)
	rg.POST("", gw.registerSkill)
	rg.POST("/:name/versions", gw.registerVersion)
	rg.POST("/:name/invoke", gw.invokeSkill)
	rg.PUT("/:name/versions/:version/traffic", gw.setTraffic)
}

// skillView 是 skill 的对外展示结构：JSON 字符串列解析为真正的 JSON 对象。
type skillView struct {
	ID           uint            `json:"id"`
	Name         string          `json:"name"`
	Owner        string          `json:"owner"`
	Runtime      string          `json:"runtime"`
	Type         string          `json:"type"`
	Entrypoint   string          `json:"entrypoint"`
	Addr         string          `json:"addr"`
	Container    string          `json:"container"`
	Limits       json.RawMessage `json:"limits"`
	Permissions  json.RawMessage `json:"permissions"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	LiveVersion  string          `json:"live_version"`
	Versions     []versionView   `json:"versions"`
	RecentRuns   []db.SkillRun   `json:"recent_runs"`
	RunCount     int64           `json:"run_count"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

type versionView struct {
	ID           uint            `json:"id"`
	Version      string          `json:"version"`
	Image        string          `json:"image"`
	TrafficSplit int             `json:"traffic_split"`
	Status       string          `json:"status"`
	RollbackTo   string          `json:"rollback_to"`
	Config       json.RawMessage `json:"config"`
	CreatedAt    string          `json:"created_at"`
}

// buildSkillView 组装 skill 详情（版本 + 最近执行 + 执行次数）。
func (g *Gateway) buildSkillView(c *gin.Context, skill *db.Skill, withRuns bool) (skillView, error) {
	view := skillView{
		ID:           skill.ID,
		Name:         skill.Name,
		Owner:        skill.Owner,
		Runtime:      skill.Runtime,
		Type:         skill.Type,
		Entrypoint:   skill.Entrypoint,
		Addr:         skill.Addr,
		Container:    skill.Container,
		Limits:       rawOrNull(skill.Limits),
		Permissions:  rawOrNull(skill.Permissions),
		InputSchema:  rawOrNull(skill.InputSchema),
		OutputSchema: rawOrNull(skill.OutputSchema),
		CreatedAt:    skill.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:    skill.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}

	versions, err := g.reg.ListVersions(c.Request.Context(), skill.ID)
	if err != nil {
		return view, err
	}
	view.Versions = make([]versionView, 0, len(versions))
	for _, v := range versions {
		view.Versions = append(view.Versions, versionView{
			ID:           v.ID,
			Version:      v.Version,
			Image:        v.Image,
			TrafficSplit: v.TrafficSplit,
			Status:       v.Status,
			RollbackTo:   v.RollbackTo,
			Config:       rawOrNull(v.Config),
			CreatedAt:    v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
		if view.LiveVersion == "" && v.Status == "live" && v.TrafficSplit > 0 {
			view.LiveVersion = v.Version
		}
	}

	if withRuns {
		runs, err := g.reg.RecentRuns(c.Request.Context(), skill.Name, 10)
		if err != nil {
			return view, err
		}
		view.RecentRuns = runs
		var count int64
		if err := g.reg.db.Model(&db.SkillRun{}).Where("skill_name = ?", skill.Name).Count(&count).Error; err != nil {
			return view, err
		}
		view.RunCount = count
	}
	return view, nil
}

// rawOrNull 把 JSON 字符串列转成 RawMessage；空串返回 "null"。
func rawOrNull(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}

// listSkills GET /api/skills
func (g *Gateway) listSkills(c *gin.Context) {
	skills, err := g.reg.ListSkills(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	views := make([]skillView, 0, len(skills))
	for i := range skills {
		view, err := g.buildSkillView(c, &skills[i], true)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		views = append(views, view)
	}
	c.JSON(http.StatusOK, gin.H{"skills": views})
}

// getSkill GET /api/skills/:name
func (g *Gateway) getSkill(c *gin.Context) {
	name := c.Param("name")
	skill, _, err := g.reg.ResolveLive(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, ErrSkillNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	view, err := g.buildSkillView(c, skill, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, view)
}

// listRuns GET /api/skills/:name/runs?limit=50
func (g *Gateway) listRuns(c *gin.Context) {
	name := c.Param("name")
	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	runs, err := g.reg.RecentRuns(c.Request.Context(), name, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

// registerSkill POST /api/skills（body = 完整 skill.json）
func (g *Gateway) registerSkill(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取请求体失败: " + err.Error()})
		return
	}
	spec, err := ValidateSpec(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	skill, version, created, err := g.reg.Register(c.Request.Context(), spec)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"skill":   skill,
		"version": version,
		"created": created,
	})
}

// registerVersion POST /api/skills/:name/versions（显式新增 draft 版本）
func (g *Gateway) registerVersion(c *gin.Context) {
	name := c.Param("name")
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取请求体失败: " + err.Error()})
		return
	}
	spec, err := ValidateSpec(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if spec.Name != name {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body 的 name 与路径不一致"})
		return
	}
	skill, version, created, err := g.reg.Register(c.Request.Context(), spec)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"skill":   skill,
		"version": version,
		"created": created,
	})
}

// invokeSkill POST /api/skills/:name/invoke（body = 输入 JSON object）
func (g *Gateway) invokeSkill(c *gin.Context) {
	name := c.Param("name")

	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取请求体失败: " + err.Error()})
		return
	}
	input := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON object: " + err.Error()})
			return
		}
	}

	meta := RunMeta{
		WorkflowID: c.GetHeader("X-Workflow-ID"),
		RunID:      c.GetHeader("X-Run-ID"),
	}

	res, err := g.runner.Invoke(c.Request.Context(), name, input, meta)
	if err != nil {
		var ie *InvokeError
		if errors.As(err, &ie) {
			body := gin.H{"error": ie.Err.Error()}
			if ie.Output != nil {
				body["output"] = ie.Output
			}
			c.JSON(ie.Code, body)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res.Output)
}

// setTraffic PUT /api/skills/:name/versions/:version/traffic
// body: {"traffic_split": 100}
func (g *Gateway) setTraffic(c *gin.Context) {
	name := c.Param("name")
	version := c.Param("version")

	var body struct {
		TrafficSplit int `json:"traffic_split"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	skill, _, err := g.reg.ResolveLive(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, ErrSkillNotFound) || errors.Is(err, ErrNoLiveVersion) {
			// 无 live 版本也能切换流量（例如给 draft 版本上线）：
			// 按名字查 skill 行拿到 skill_id。
			var s db.Skill
			if qerr := g.reg.db.Where("name = ?", name).First(&s).Error; qerr != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": ErrSkillNotFound.Error()})
				return
			}
			skill = &s
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	v, err := g.reg.SetTraffic(c.Request.Context(), skill.ID, version, body.TrafficSplit)
	if err != nil {
		if errors.Is(err, ErrVersionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, v)
}
