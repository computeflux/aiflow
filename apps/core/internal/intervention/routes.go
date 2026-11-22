// Package intervention 是 S3 干预舱 API（takeover/inject/retry/rollback/resolve + 活跃 workflow/熔断查询）。
package intervention

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/circuitbreaker"
	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
	"github.com/wetee/ai-workflow/internal/temporalscan"
	"github.com/wetee/ai-workflow/internal/workflows"
)

// Deps 是干预路由的依赖集（S3 §3.3）。
type Deps struct {
	Store    *Store
	DefaultC client.Client // default namespace（存量 workflow）
	AgentC   client.Client // agent-prod namespace（S2 新 workflow）
	Breaker  *circuitbreaker.Registry
	Approval *approval.Store // audit_logs 留痕 + rollback 审批单
	Notifier notify.Notifier
	Cfg      *config.Config // 队列名
}

// RegisterRoutes 挂载 /interventions/* 与干预舱辅助查询路由（mws 传 requireAuth）。
func RegisterRoutes(r gin.IRouter, d *Deps, mws ...gin.HandlerFunc) {
	g := r.Group("/interventions", mws...)
	g.GET("", d.list)
	g.POST("/:id/takeover", d.takeover)
	g.POST("/:id/inject", d.inject)
	g.POST("/:id/retry", d.retry)
	g.POST("/:id/rollback", d.rollback)
	g.POST("/:id/resolve", d.resolve)
	// board 干预舱页辅助查询（同组鉴权）
	g.GET("/active-workflows", d.activeWorkflows)
	g.GET("/circuit", d.circuitStates)
}

// ---- 查询 ----

func (d *Deps) list(c *gin.Context) {
	items, err := d.Store.List(c, c.Query("status"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// activeWorkflows GET /interventions/active-workflows —— 活跃 workflow 表
// （id、开始时间、事件数、最近错误；熔断圆点走 /interventions/circuit）。
func (d *Deps) activeWorkflows(c *gin.Context) {
	items := []temporalscan.WorkflowScan{}
	type clientNS struct {
		c  client.Client
		ns string
	}
	for _, p := range []clientNS{
		{d.AgentC, d.Cfg.TemporalAgentNamespace},
		{d.DefaultC, d.Cfg.TemporalNamespace},
	} {
		scans, err := temporalscan.ScanRunning(c, p.c, p.ns, 30)
		if err != nil {
			continue // 单 namespace 扫描失败不阻断
		}
		items = append(items, scans...)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// circuitStates GET /interventions/circuit —— 全部已知 skill 的熔断状态（board 圆点）。
func (d *Deps) circuitStates(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"items": d.Breaker.All()})
}

// ---- 干预动作 ----

// findClient 按 workflow_id 定位所在 namespace（先 agent-prod 后 default）。
func (d *Deps) findClient(ctx context.Context, wid string) client.Client {
	if wid == "" {
		return nil
	}
	for _, cc := range []client.Client{d.AgentC, d.DefaultC} {
		_, err := cc.DescribeWorkflowExecution(ctx, wid, "")
		if err == nil {
			return cc
		}
	}
	return nil
}

type actionRequest struct {
	Workflow string         `json:"workflow"` // weekly_content | fetch | generic_skill
	Input    map[string]any `json:"input"`
	Skill    string         `json:"skill"` // retry 可选：重置该 skill 的 breaker
}

// startable 是可重启的 workflow 类型注册表（v1 三类，S3 §3.3 takeover/retry）。
type startable struct {
	fn     interface{}
	queue  string
	client client.Client
	decode func(body []byte) (interface{}, error)
}

func (d *Deps) startables() map[string]startable {
	return map[string]startable{
		"weekly_content": {
			fn:     workflows.WeeklyContentWorkflow,
			queue:  d.Cfg.FlexTaskQueue,
			client: d.AgentC,
			decode: decodeInto[workflows.WeeklyContentInput],
		},
		"fetch": {
			fn:     workflows.FetchWorkflow,
			queue:  d.Cfg.TemporalGoTaskQueue,
			client: d.DefaultC,
			decode: decodeInto[workflows.FetchWorkflowInput],
		},
		"generic_skill": {
			fn:     workflows.GenericSkillWorkflow,
			queue:  d.Cfg.SkillTaskQueue,
			client: d.AgentC,
			decode: decodeInto[workflows.GenericSkillInput],
		},
	}
}

func decodeInto[T any](body []byte) (interface{}, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("decode input as %T: %w", v, err)
	}
	// workflow 函数按值接收 input（如 GenericSkillWorkflow(ctx, input GenericSkillInput)）
	return v, nil
}

// cancelAndStart 取消旧 workflow 并以修正参数启动新 workflow（takeover/retry 共用）。
// 返回新 workflow id；旧 workflow 不在任何 namespace 时跳过取消（仍启动新 workflow）。
func (d *Deps) cancelAndStart(c *gin.Context, item *db.Intervention, req actionRequest) (string, error) {
	s, ok := d.startables()[req.Workflow]
	if !ok {
		return "", fmt.Errorf("unsupported workflow type %q (weekly_content/fetch/generic_skill)", req.Workflow)
	}
	b, merr := json.Marshal(req.Input)
	if merr != nil {
		return "", merr
	}
	in, err := s.decode(b)
	if err != nil {
		return "", err
	}

	if cc := d.findClient(c, item.WorkflowID); cc != nil {
		if err := cc.CancelWorkflow(c, item.WorkflowID, ""); err != nil {
			return "", fmt.Errorf("cancel old workflow %s: %w", item.WorkflowID, err)
		}
	}

	newID := fmt.Sprintf("%s-%s-%s", req.Workflow, item.WorkflowID, uuid.NewString())
	wr, err := s.client.ExecuteWorkflow(c, client.StartWorkflowOptions{
		TaskQueue:                s.queue,
		ID:                       newID,
		WorkflowExecutionTimeout: 24 * time.Hour,
	}, s.fn, in)
	if err != nil {
		return "", fmt.Errorf("start new workflow: %w", err)
	}
	return wr.GetID(), nil
}

// audit 写 audit_logs（actor=操作人，S3 §3.3 全部干预动作留痕）。
func (d *Deps) audit(c *gin.Context, action, target string, detail map[string]any) {
	d.Approval.Log(c, authn.UserID(c), action, target, detail)
}

// takeover POST /interventions/:id/takeover —— 人工接管：
// 取消当前 workflow，用修正参数启动新 workflow。
func (d *Deps) takeover(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req actionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	item, err := d.Store.Get(c, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "intervention not found"})
		return
	}
	newID, err := d.cancelAndStart(c, item, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	d.audit(c, "takeover", item.WorkflowID, map[string]any{
		"intervention_id": id, "workflow": req.Workflow, "new_workflow_id": newID,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "old_workflow_id": item.WorkflowID, "new_workflow_id": newID})
}

type injectRequest struct {
	Name    string         `json:"name"`
	Payload map[string]any `json:"payload"`
}

// inject POST /interventions/:id/inject —— Signal 注入修正上下文（白名单信号，复用 /runs 白名单）。
func (d *Deps) inject(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req injectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if !approval.IsSignalAllowed(req.Name) {
		d.audit(c, "inject_rejected", strconv.FormatUint(uint64(id), 10), map[string]any{"name": req.Name})
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("signal %q not in whitelist (approve/reject/inject)", req.Name)})
		return
	}
	item, err := d.Store.Get(c, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "intervention not found"})
		return
	}
	cc := d.findClient(c, item.WorkflowID)
	if cc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found in agent-prod/default"})
		return
	}
	if err := cc.SignalWorkflow(c, item.WorkflowID, "", req.Name, req.Payload); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "signal failed: " + err.Error()})
		return
	}
	d.audit(c, "inject", item.WorkflowID, map[string]any{
		"intervention_id": id, "name": req.Name, "payload": req.Payload,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "workflow_id": item.WorkflowID, "signal": req.Name})
}

// retry POST /interventions/:id/retry —— 重试：重置 breaker（可选 skill）+ 重新调度。
func (d *Deps) retry(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req actionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	item, err := d.Store.Get(c, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "intervention not found"})
		return
	}
	if req.Skill != "" {
		d.Breaker.Get(req.Skill).Reset(c)
	}
	newID, err := d.cancelAndStart(c, item, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	d.audit(c, "retry", item.WorkflowID, map[string]any{
		"intervention_id": id, "workflow": req.Workflow, "skill": req.Skill, "new_workflow_id": newID,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "new_workflow_id": newID})
}

type rollbackRequest struct {
	SkillName  string `json:"skill_name"`
	RollbackTo string `json:"rollback_to"`
}

// rollback POST /interventions/:id/rollback —— skill 版本回滚（**走 S1 审批**，不直接执行）。
func (d *Deps) rollback(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req rollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.SkillName == "" || req.RollbackTo == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill_name + rollback_to required"})
		return
	}
	if _, err := d.Store.Get(c, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "intervention not found"})
		return
	}

	// 去重：同 skill 已有 pending 的 rollback 审批单则不重复创建
	var dup int64
	_ = d.ApprovalDedupCheck(c, req.SkillName, &dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "pending rollback approval already exists for this skill"})
		return
	}

	// 当前 live 版本（回滚目标记录在 payload.version，与 S1 RunCD 的 cdPayload 对齐）
	var liveVersion string
	var skill db.Skill
	if err := d.Store.database.WithContext(c).Where("name = ?", req.SkillName).First(&skill).Error; err == nil {
		var v db.SkillVersion
		_ = d.Store.database.WithContext(c).
			Where("skill_id = ? AND status = ? AND traffic_split > 0 ORDER BY traffic_split DESC", skill.ID, "live").
			First(&v).Error
		if v.Version != "" {
			liveVersion = v.Version
		}
	}
	payload := map[string]any{
		"skill_name":  req.SkillName,
		"version":     liveVersion,
		"rollback_to": req.RollbackTo,
	}
	b, _ := json.Marshal(payload)
	now := time.Now()
	expiresAt := now.Add(24 * time.Hour)
	// approval_no 列是 varchar(32)：用 秒级时间戳 + 4 位 hex 后缀（skill 名在 subject/payload，不进编号）
	approvalNo := fmt.Sprintf("APR-INT-%d-%s", now.Unix(), strings.ToLower(uuid.NewString()[:4]))
	apv := &db.Approval{
		ApprovalNo: approvalNo,
		Kind:       "rollback",
		Subject:    fmt.Sprintf("干预回滚：%s → v%s（intervention #%d）", req.SkillName, req.RollbackTo, id),
		Payload:    string(b),
		Status:     "pending",
		Proposer:   authn.UserID(c),
		ExpiresAt:  &expiresAt,
		CreatedAt:  now,
	}
	if err := d.Store.database.WithContext(c).Create(apv).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create rollback approval: " + err.Error()})
		return
	}
	d.audit(c, "rollback", req.SkillName, map[string]any{
		"intervention_id": id, "rollback_to": req.RollbackTo, "approval_no": approvalNo,
	})
	// S4 §3.2：回滚审批单产生 → #approvals（approval.request 协议）
	_ = d.Notifier.SendApproval(c, notify.ApprovalNotice{
		ID: apv.ID, ApprovalNo: approvalNo, Kind: "rollback",
		Subject: apv.Subject, Proposer: authn.UserID(c), ExpiresAt: expiresAt,
		Summary: fmt.Sprintf("rollback to v%s", req.RollbackTo),
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "approval_no": approvalNo})
}

// resolve POST /interventions/:id/resolve —— 标记 resolved。
func (d *Deps) resolve(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req) // 空 body 也允许
	item, err := d.Store.Resolve(c, id, req.Note)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "intervention not found"})
		return
	}
	d.audit(c, "resolve", strconv.FormatUint(uint64(id), 10), map[string]any{"note": req.Note})
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": item.ID, "status": item.Status})
}

func parseID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return uint(id), true
}

// ApprovalDedupCheck 供 rollback 去重查询（查 approvals 表）。
func (d *Deps) ApprovalDedupCheck(ctx context.Context, skillName string, dup *int64) error {
	return d.Store.database.WithContext(ctx).Model(&db.Approval{}).
		Where("kind = ? AND status = ? AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.skill_name')) = ?",
			"rollback", "pending", skillName).
		Count(dup).Error
}
