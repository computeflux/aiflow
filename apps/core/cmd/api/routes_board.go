package main

import (
	"github.com/gin-gonic/gin"
)

// registerBoardRoutes 产品板块看板（board）路由注册入口：Board / List / Card / 评论 /
// 执行记录 / 重跑 / 手动收口 / 看板直接审批 / 跳过审核 / Board AI / 思维导图（含 AI 对话）。
// 批次 F 拆分：各域实现见 routes_board_{crud,lifecycle,mindmap}.go（行为无变更）。
func registerBoardRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	registerBoardCrudRoutes(r, deps, requireAuth, requireRole)
	registerBoardCardLifecycleRoutes(r, deps, requireAuth, requireRole)
	registerBoardMindmapRoutes(r, deps, requireAuth, requireRole)
}
