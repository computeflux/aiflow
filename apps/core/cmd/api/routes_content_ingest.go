package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/otel"
	"github.com/wetee/ai-workflow/internal/workflows"
	"go.temporal.io/sdk/client"
)

// registerContentIngestRoutes 内容生产域·采集任务（ingest-tasks CRUD / run / spread /
// restart-crons）。逻辑自 registerContentRoutes 原样搬迁（批次 F 拆分，行为无变更）。
func registerContentIngestRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database
	c := deps.temporal
	cfg := deps.cfg

	r.GET("/ingest-tasks", requireAuth(), func(ctx *gin.Context) {
		page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
		pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
		if page < 1 {
			page = 1
		}
		if pageSize < 1 || pageSize > 100 {
			pageSize = 20
		}
		var total int64
		database.Model(&db.IngestTask{}).Count(&total)
		var tasks []db.IngestTask
		database.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&tasks)
		ctx.JSON(http.StatusOK, gin.H{"tasks": tasks, "total": total})
	})

	r.POST("/ingest-tasks", requireAuth(), func(ctx *gin.Context) {
		var input struct {
			Source, Keyword string
			Limit           int
			Mode, Schedule  string
			ParallelCount   int
			Enabled         *bool
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		task := db.IngestTask{Source: input.Source, Keyword: input.Keyword, Limit: input.Limit, Mode: input.Mode, Schedule: input.Schedule, ParallelCount: input.ParallelCount, Enabled: true}
		if input.Enabled != nil {
			task.Enabled = *input.Enabled
		}
		database.Create(&task)
		if task.Schedule != "" && task.Enabled {
			if err := startCron(c, task, cfg.TemporalGoTaskQueue); err != nil {
				log.Printf("startCron task %d create: %v", task.ID, err)
				otel.Record(database, otel.Entry{
					System:  "api",
					Source:  "startCron",
					Message: fmt.Sprintf("start cron workflow for task %d failed: %v", task.ID, err),
					Context: fmt.Sprintf(`{"task_id":%d,"source":"%s","keyword":"%s"}`, task.ID, task.Source, task.Keyword),
				})
			}
		}
		ctx.JSON(http.StatusCreated, gin.H{"task": task})
	})

	r.PUT("/ingest-tasks/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var task db.IngestTask
		if err := database.First(&task, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		var input struct {
			Source, Keyword, Mode, Schedule *string
			Limit, ParallelCount            *int
			Enabled                         *bool
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := map[string]any{"updated_at": time.Now()}
		if input.Source != nil {
			updates["source"] = *input.Source
		}
		if input.Keyword != nil {
			updates["keyword"] = *input.Keyword
		}
		if input.Limit != nil {
			updates["limit"] = *input.Limit
		}
		if input.ParallelCount != nil {
			updates["parallel_count"] = *input.ParallelCount
		}
		if input.Mode != nil {
			updates["mode"] = *input.Mode
		}
		if input.Schedule != nil {
			updates["schedule"] = *input.Schedule
		}
		if input.Enabled != nil {
			updates["enabled"] = *input.Enabled
		}
		database.Model(&task).Updates(updates)
		database.First(&task, id)
		if task.Schedule != "" && task.Enabled {
			if err := startCron(c, task, cfg.TemporalGoTaskQueue); err != nil {
				log.Printf("startCron task %d update: %v", task.ID, err)
				otel.Record(database, otel.Entry{
					System:  "api",
					Source:  "startCron",
					Message: fmt.Sprintf("restart cron workflow for task %d failed: %v", task.ID, err),
					Context: fmt.Sprintf(`{"task_id":%d,"source":"%s","keyword":"%s"}`, task.ID, task.Source, task.Keyword),
				})
			}
		} else {
			stopCron(c, task)
		}
		ctx.JSON(http.StatusOK, gin.H{"task": task})
	})

	r.DELETE("/ingest-tasks/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var task db.IngestTask
		if err := database.First(&task, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		stopCron(c, task)
		database.Delete(&task)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	r.POST("/ingest-tasks/:id/run", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var task db.IngestTask
		if err := database.First(&task, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: cfg.TemporalGoTaskQueue, ID: fmt.Sprintf("ingest-%s-%s", task.Source, uuid.NewString())}, workflows.FetchWorkflow, workflows.FetchWorkflowInput{Source: task.Source, Keyword: task.Keyword, Limit: task.Limit, Mode: task.Mode, ParallelCount: task.ParallelCount})
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		database.Model(&task).Updates(map[string]any{"last_run_at": time.Now(), "last_workflow_id": wr.GetID()})
		ctx.JSON(http.StatusOK, gin.H{"workflow_id": wr.GetID(), "run_id": wr.GetRunID()})
	})

	// Spread cron tasks evenly across the hour
	r.POST("/ingest-tasks/spread", requireAuth(), func(ctx *gin.Context) {
		var input struct {
			Source string `json:"source"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.Source == "" {
			input.Source = "x"
		}

		var tasks []db.IngestTask
		database.Where("source = ? AND schedule != '' AND enabled = true", input.Source).Find(&tasks)
		if len(tasks) < 2 {
			ctx.JSON(http.StatusOK, gin.H{"message": "less than 2 tasks, nothing to spread", "count": len(tasks)})
			return
		}

		// Distribute minute offsets evenly
		n := len(tasks)
		step := float64(60) / float64(n)
		for i, task := range tasks {
			parts := strings.Fields(task.Schedule)
			if len(parts) == 5 {
				parts[0] = strconv.Itoa(int(math.Round(float64(i)*step)) % 60)
				newSchedule := strings.Join(parts, " ")
				if newSchedule != task.Schedule {
					task.Schedule = newSchedule
					database.Model(&task).Updates(map[string]any{"schedule": newSchedule, "updated_at": time.Now()})
					if err := startCron(c, task, cfg.TemporalGoTaskQueue); err != nil {
						log.Printf("startCron task %d spread: %v", task.ID, err)
						otel.Record(database, otel.Entry{
							System:  "api",
							Source:  "startCron",
							Message: fmt.Sprintf("spread: restart cron workflow for task %d failed: %v", task.ID, err),
							Context: fmt.Sprintf(`{"task_id":%d,"source":"%s"}`, task.ID, task.Source),
						})
					}
				}
			}
		}

		ctx.JSON(http.StatusOK, gin.H{"message": "spread complete", "count": len(tasks), "step": step})
	})

	// Restart all cron workflows for enabled scheduled tasks
	r.POST("/ingest-tasks/restart-crons", requireRole("admin"), func(ctx *gin.Context) {
		var tasks []db.IngestTask
		database.Where("schedule != '' AND enabled = true").Find(&tasks)
		var success, failed int
		for _, task := range tasks {
			if err := startCron(c, task, cfg.TemporalGoTaskQueue); err != nil {
				log.Printf("restart-crons task %d: %v", task.ID, err)
				failed++
			} else {
				success++
			}
		}
		ctx.JSON(http.StatusOK, gin.H{"total": len(tasks), "success": success, "failed": failed})
	})
}

// startCron 创建/更新 ingest 采集 cron workflow（先取消同名旧 workflow 再按新 schedule 启动）。
// 逻辑自 main() 原样搬迁（批次 A 重构，行为无变更）。
func startCron(c client.Client, task db.IngestTask, taskQueue string) error {
	wid := fmt.Sprintf("scraper-%s-%d", task.Source, task.ID)
	_ = c.CancelWorkflow(context.Background(), wid, "")
	_, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		TaskQueue: taskQueue, ID: wid, CronSchedule: task.Schedule,
		WorkflowExecutionTimeout: 30 * 24 * time.Hour,
	}, workflows.ScheduledFetchWorkflow, workflows.ScheduledFetchWorkflowInput{
		Source: task.Source, Keyword: task.Keyword, Limit: task.Limit, Mode: task.Mode, ParallelCount: task.ParallelCount,
	})
	return err
}

// stopCron 取消 ingest 采集 cron workflow。逻辑自 main() 原样搬迁（批次 A 重构，行为无变更）。
func stopCron(c client.Client, task db.IngestTask) {
	_ = c.CancelWorkflow(context.Background(), fmt.Sprintf("scraper-%s-%d", task.Source, task.ID), "")
}
