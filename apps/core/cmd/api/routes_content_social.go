package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/otel"
	"github.com/wetee/ai-workflow/internal/workflows"
	"go.temporal.io/sdk/client"
)

// registerContentSocialRoutes 内容生产域·排程与分发（social-schedules / social-tweets /
// follow-targets dispatch）。逻辑自 registerContentRoutes 原样搬迁（批次 F 拆分，行为无变更）。
func registerContentSocialRoutes(
	r *gin.Engine,
	deps *appDeps,
	cronClient client.Client,
	cronQueue string,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database
	c := deps.temporal
	cfg := deps.cfg

	// Social schedule management
	r.POST("/social-schedules", func(ctx *gin.Context) {
		var sched db.SocialSchedule
		if err := ctx.ShouldBindJSON(&sched); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		sched.Enabled = true
		database.Create(&sched)
		// Start cron workflow（S2：CRON_QUEUE_MIGRATED 时走 agent-prod 的 cron 队列）
		wid := fmt.Sprintf("social-sched-%d", sched.ID)
		_, err := cronClient.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			TaskQueue: cronQueue, ID: wid, CronSchedule: sched.Schedule,
			WorkflowExecutionTimeout: 30 * 24 * time.Hour,
		}, workflows.ScheduledAutoTweetWorkflow, workflows.AutoTweetInput{
			Hours: sched.Hours, Count: sched.Count, Source: sched.Sources,
		})
		if err != nil {
			database.Delete(&sched)
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusCreated, sched)
	})

	r.GET("/social-schedules", requireAuth(), func(ctx *gin.Context) {
		var scheds []db.SocialSchedule
		database.Order("id desc").Find(&scheds)
		ctx.JSON(http.StatusOK, scheds)
	})

	r.DELETE("/social-schedules/:id", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		// S2：与创建侧同一 client/队列（CRON_QUEUE_MIGRATED 时走 agent-prod）
		cronClient.CancelWorkflow(context.Background(), fmt.Sprintf("social-sched-%d", id), "")
		database.Delete(&db.SocialSchedule{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	r.GET("/social-tweets", requireAuth(), func(ctx *gin.Context) {
		var tweets []db.SocialTweet
		database.Order("id desc").Limit(50).Find(&tweets)
		ctx.JSON(http.StatusOK, tweets)
	})

	// 手动触发某订阅目标的分发：扫描推文并为所有机器生成随机时间的点赞任务
	r.POST("/follow-targets/:id/dispatch", requireRole("admin"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var target db.FollowTarget
		if err := database.First(&target, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "target not found"})
			return
		}
		wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			TaskQueue: cfg.TemporalGoTaskQueue,
			ID:        fmt.Sprintf("follow-like-dispatch-%d-%s", id, uuid.NewString()),
		}, workflows.FollowLikeDispatchWorkflow, workflows.FollowLikeDispatchInput{
			TargetID:          uint(id),
			PyreplayTaskQueue: cfg.TemporalPyreplayTaskQueue,
		})
		if err != nil {
			otel.Record(database, otel.Entry{
				System:  "api",
				Source:  "follow-targets dispatch",
				Message: fmt.Sprintf("dispatch follow target %d failed: %v", id, err),
			})
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusAccepted, gin.H{"workflow_id": wr.GetID(), "run_id": wr.GetRunID()})
	})
}
