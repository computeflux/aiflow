package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workflows"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// registerContentPublishRoutes 内容生产域·发布与状态（jobs / raw-items / vector-points /
// 审批发布 / status）。逻辑自 registerContentRoutes 原样搬迁（批次 F 拆分，行为无变更）。
func registerContentPublishRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database
	c := deps.temporal
	cfg := deps.cfg

	r.GET("/jobs", requireAuth(), func(ctx *gin.Context) {
		status := ctx.Query("status")
		var jobs []db.PublishJob
		query := database.Order("created_at desc")
		if status != "" {
			query = query.Where("status = ?", status)
		}
		query.Find(&jobs)
		ctx.JSON(http.StatusOK, jobs)
	})

	r.GET("/raw-items", requireAuth(), func(ctx *gin.Context) {
		source := ctx.Query("source")
		page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
		pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
		if page < 1 {
			page = 1
		}
		if pageSize < 1 || pageSize > 100 {
			pageSize = 20
		}

		var total int64
		query := database.Model(&db.RawItem{})
		if source != "" {
			query = query.Where("source = ?", source)
		}
		query.Count(&total)

		var items []db.RawItem
		query.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items)
		ctx.JSON(http.StatusOK, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
	})

	r.GET("/vector-points", requireAuth(), func(ctx *gin.Context) {
		pts, err := deps.qdrantClient.Scroll(ctx, 50)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, serverError(database, "vector-points", err))
			return
		}
		result := []gin.H{}
		for _, p := range pts {
			payload := map[string]any{}
			for k, v := range p.Payload {
				if v != nil {
					if v.GetStringValue() != "" {
						payload[k] = v.GetStringValue()
					} else {
						payload[k] = v.String()
					}
				}
			}
			result = append(result, gin.H{"id": p.GetId().GetUuid(), "payload": payload})
		}
		ctx.JSON(http.StatusOK, result)
	})

	// 审批/发布（敏感操作，需登录 + 审批角色；未部署 auth 时直通）
	r.POST("/jobs/:id/approve", requireRole("admin", "board_approver"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		updateJobStatus(database, uint(id), db.StatusApproved, "")
		ctx.JSON(http.StatusOK, gin.H{"status": db.StatusApproved})
	})

	r.POST("/jobs/:id/reject", requireRole("admin", "board_approver"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var body struct {
			Reason string `json:"reason"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updateJobStatus(database, uint(id), db.StatusRejected, body.Reason)
		ctx.JSON(http.StatusOK, gin.H{"status": db.StatusRejected})
	})

	r.POST("/publish", requireRole("admin", "board_approver"), func(ctx *gin.Context) {
		var input workflows.PublishWorkflowInput
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: cfg.TemporalGoTaskQueue, ID: fmt.Sprintf("publish-job-%s", uuid.NewString())}, workflows.PublishWorkflow, input)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"workflow_id": wr.GetID(), "run_id": wr.GetRunID()})
	})

	r.GET("/status", requireAuth(), func(ctx *gin.Context) {
		var rawCount, jobCount, acctCount int64
		database.Model(&db.RawItem{}).Count(&rawCount)
		database.Model(&db.PublishJob{}).Count(&jobCount)
		database.Model(&db.Account{}).Count(&acctCount)
		var pending, approved, published, failed int64
		database.Model(&db.PublishJob{}).Where("status = ?", "pending").Count(&pending)
		database.Model(&db.PublishJob{}).Where("status = ?", "approved").Count(&approved)
		database.Model(&db.PublishJob{}).Where("status = ?", "published").Count(&published)
		database.Model(&db.PublishJob{}).Where("status = ?", "failed").Count(&failed)
		active := []gin.H{}
		if resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Namespace: cfg.TemporalNamespace, Query: "ExecutionStatus = 'Running'"}); err == nil {
			for _, exec := range resp.Executions {
				var st string
				if exec.StartTime != nil {
					st = exec.StartTime.AsTime().Format(time.RFC3339)
				}
				sn := strings.TrimPrefix(enums.WorkflowExecutionStatus_name[int32(exec.Status)], "WORKFLOW_EXECUTION_STATUS_")
				active = append(active, gin.H{"workflow_id": exec.Execution.WorkflowId, "run_id": exec.Execution.RunId, "type": exec.Type.Name, "start_time": st, "status": sn})
			}
		}
		ctx.JSON(http.StatusOK, gin.H{"database": gin.H{"total_raw_items": rawCount, "total_jobs": jobCount, "total_accounts": acctCount, "jobs_stat": gin.H{"pending": pending, "approved": approved, "published": published, "failed": failed}}, "active_tasks": active})
	})
}
