package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/crag"
	"github.com/wetee/ai-workflow/internal/product"
	"github.com/wetee/ai-workflow/internal/qdrant"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
)

// registerContentInspectRoutes 内容生产域·检索与 workflow 观测（search / workflows 列表与
// 取消/终止）。逻辑自 registerContentRoutes 原样搬迁（批次 F 拆分，行为无变更）。
func registerContentInspectRoutes(
	r *gin.Engine,
	deps *appDeps,
	cragEngine *crag.CRAG,
	qdrantClient *qdrant.Client,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database
	c := deps.temporal
	cfg := deps.cfg

	r.GET("/search", requireAuth(), func(ctx *gin.Context) {
		q := ctx.Query("q")
		if q == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "missing q"})
			return
		}
		k, _ := strconv.Atoi(ctx.DefaultQuery("k", "5"))
		days, _ := strconv.Atoi(ctx.DefaultQuery("days", "7"))
		// S7 分层授权：按用户角色过滤可见密级（普通用户只见 public，admin 见 internal，白名单角色见 secret）
		result, err := cragEngine.SearchFiltered(ctx, q, k, days, product.ConfidentialityLevelsFor(authn.Roles(ctx)))
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, serverError(database, "search", err))
			return
		}
		ctx.JSON(http.StatusOK, result)
	})

	r.GET("/workflows", requireAuth(), func(ctx *gin.Context) {
		status := ctx.DefaultQuery("status", "")
		var query string
		switch status {
		case "running":
			query = "ExecutionStatus = 'Running'"
		case "completed":
			query = "ExecutionStatus = 'Completed'"
		case "failed":
			query = "ExecutionStatus = 'Failed'"
		default:
			query = "StartTime >= '2020-01-01T00:00:00Z'"
		}
		resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Namespace: cfg.TemporalNamespace, Query: query, PageSize: 50})
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		items := []gin.H{}
		for _, exec := range resp.Executions {
			var st, ct string
			if exec.StartTime != nil {
				st = exec.StartTime.AsTime().Format(time.RFC3339)
			}
			if exec.CloseTime != nil {
				ct = exec.CloseTime.AsTime().Format(time.RFC3339)
			}
			sn := strings.TrimPrefix(enums.WorkflowExecutionStatus_name[int32(exec.Status)], "WORKFLOW_EXECUTION_STATUS_")
			items = append(items, gin.H{"workflow_id": exec.Execution.WorkflowId, "run_id": exec.Execution.RunId, "type": exec.Type.Name, "start_time": st, "close_time": ct, "status": sn, "history_length": exec.HistoryLength})
		}
		ctx.JSON(http.StatusOK, gin.H{"workflows": items})
	})

	// Cancel a running workflow
	r.POST("/workflows/:id/cancel", requireAuth(), func(ctx *gin.Context) {
		wid := ctx.Param("id")
		runID := ctx.DefaultQuery("run_id", "")
		err := c.CancelWorkflow(context.Background(), wid, runID)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"message": "cancelled"})
	})

	// Terminate a running workflow
	r.POST("/workflows/:id/terminate", requireRole("admin"), func(ctx *gin.Context) {
		wid := ctx.Param("id")
		runID := ctx.DefaultQuery("run_id", "")
		err := c.TerminateWorkflow(context.Background(), wid, runID, "user request")
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"message": "terminated"})
	})
}
