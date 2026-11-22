package main

import (
	"github.com/gin-gonic/gin"
)

// registerContentRoutes 内容生产域（MCP→RAG→发布主链路）路由注册入口：
// 生成/点赞/skill 调用/周内容、社交排程、X 账号与订阅目标管理、检索、
// Temporal workflow 查询与取消、采集任务（ingest-tasks）、发布审批、状态总览。
// 批次 F 拆分：各域实现见 routes_content_{skill,social,x,inspect,ingest,publish}.go（行为无变更）。
func registerContentRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	cfg, database, cragEngine := deps.cfg, deps.database, deps.cragEngine
	c, agentC := deps.temporal, deps.temporalAgent
	// S2 混合编排（docs/plan/S2-混合编排.md §3.1）：cron 队列迁移后走 agent-prod。
	cronClient, cronQueue := c, cfg.TemporalGoTaskQueue
	if cfg.CronQueueMigrated {
		cronClient, cronQueue = agentC, cfg.CronTaskQueue
	}
	_ = database

	registerContentSkillRoutes(r, deps, agentC, requireAuth, requireRole)

	registerContentSocialRoutes(r, deps, cronClient, cronQueue, requireAuth, requireRole)

	registerContentXRoutes(r, deps, requireAuth, requireRole)

	registerContentInspectRoutes(r, deps, cragEngine, deps.qdrantClient, requireAuth, requireRole)

	registerContentIngestRoutes(r, deps, requireAuth, requireRole)

	registerContentPublishRoutes(r, deps, requireAuth, requireRole)
}
