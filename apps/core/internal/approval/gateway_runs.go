package approval

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.temporal.io/sdk/client"
)

// signalWhitelist 是 /runs/* 允许发送的 Signal 白名单（S2 §3.4：代码内 const 列表，非法 403）。
var signalWhitelist = map[string]bool{
	"approve": true,
	"reject":  true,
	"inject":  true,
}

// IsSignalAllowed 判断信号是否在白名单内（/runs/signal 与 /interventions/:id/inject 共用）。
func IsSignalAllowed(name string) bool { return signalWhitelist[name] }

// Runs 是 /runs/* 路由集（S2 §3.4 Agent 权限收口）。
// 操作对象为 agent-prod namespace（S2 新 workflow 所在）；
// 每次操作写 audit_logs 留痕（只增不删，S1 约定）。
type Runs struct {
	agentC client.Client
	store  *Store
}

// NewRuns 构造 /runs/* 路由集（agent-prod client + 审批存储）。
func NewRuns(agentC client.Client, store *Store) *Runs {
	return &Runs{agentC: agentC, store: store}
}

// RegisterRoutes 注册 /runs/* 路由（mws 传 requireAuth 等中间件）。
func RegisterRunsRoutes(r gin.IRouter, rs *Runs, mws ...gin.HandlerFunc) {
	g := r.Group("/runs", mws...)
	g.GET("/:workflow_id", rs.query)
	g.POST("/:workflow_id/signal", rs.signal)
	g.POST("/:workflow_id/cancel", rs.cancel)
}

// query GET /runs/:workflow_id —— 查询执行状态（只读）。
func (rs *Runs) query(c *gin.Context) {
	wid := c.Param("workflow_id")
	desc, err := rs.agentC.DescribeWorkflowExecution(c, wid, "")
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found: " + err.Error()})
		return
	}
	info := desc.WorkflowExecutionInfo
	// api proto 的 WorkflowExecutionInfo：Execution 嵌套结构持有 WorkflowId/RunId（防御性 nil 保护）
	out := gin.H{
		"status":     info.Status.String(),
		"task_queue": info.TaskQueue,
	}
	if info.Execution != nil {
		out["workflow_id"] = info.Execution.WorkflowId
		out["run_id"] = info.Execution.RunId
	}
	if info.StartTime != nil {
		out["start_time"] = info.StartTime.AsTime().Format(time.RFC3339)
	}
	if info.CloseTime != nil {
		out["close_time"] = info.CloseTime.AsTime().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, out)
}

type signalRequest struct {
	Name    string         `json:"name"`
	Payload map[string]any `json:"payload"`
}

// signal POST /runs/:workflow_id/signal —— 发 Signal（白名单，非法 403）。
func (rs *Runs) signal(c *gin.Context) {
	wid := c.Param("workflow_id")
	var req signalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if !IsSignalAllowed(req.Name) {
		rs.store.Log(c, "runs-api", "signal_rejected", wid, map[string]any{"name": req.Name})
		c.JSON(http.StatusForbidden, gin.H{
			"error": fmt.Sprintf("signal %q not in whitelist (approve/reject/inject)", req.Name),
		})
		return
	}
	if err := rs.agentC.SignalWorkflow(c, wid, "", req.Name, req.Payload); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "signal failed: " + err.Error()})
		return
	}
	rs.store.Log(c, "runs-api", "signal_sent", wid, map[string]any{"name": req.Name, "payload": req.Payload})
	c.JSON(http.StatusOK, gin.H{"ok": true, "workflow_id": wid, "signal": req.Name})
}

// cancel POST /runs/:workflow_id/cancel —— 请求取消。
func (rs *Runs) cancel(c *gin.Context) {
	wid := c.Param("workflow_id")
	if err := rs.agentC.CancelWorkflow(c, wid, ""); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cancel failed: " + err.Error()})
		return
	}
	rs.store.Log(c, "runs-api", "cancel_requested", wid, nil)
	c.JSON(http.StatusOK, gin.H{"ok": true, "workflow_id": wid})
}
