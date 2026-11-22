package approval

import (
	"github.com/gin-gonic/gin"
)

// RegisterRoutes 注册审批 / Agent / 进化单路由（无前缀风格，与 legacy 一致；
// board 经 nginx /api 同源代理剥前缀后到达）。
// P4-1：新增的 audit-logs 路由必须鉴权（mws 传 requireAuth，与 gateway /
// intervention 同款），防止未授权方伪造审计留痕 / 拉取全量审计 detail；
// 既有审批路由保持原样（行为不变）。
// RegisterRoutes 注册审批 / Agent / 进化单路由（无前缀风格，与 legacy 一致；
// board 经 nginx /api 同源代理剥前缀后到达）。
// P4-1：新增的 audit-logs 路由必须鉴权（mws 传 requireAuth，与 gateway /
// intervention 同款），防止未授权方伪造审计留痕 / 拉取全量审计 detail；
// 既有审批路由保持原样（行为不变）。
func RegisterRoutes(r *gin.Engine, s *Store, mws ...gin.HandlerFunc) {
	r.GET("/approvals", s.listApprovals)
	r.GET("/approvals/:id", s.getApproval)
	r.POST("/approvals/:id/approve", s.approve)
	r.POST("/approvals/:id/reject", s.reject)

	r.GET("/agents", s.listAgents)
	r.POST("/agents", s.registerAgent)
	r.POST("/agents/:role/bind", s.bindAgent)
	// S8.2 agent 运行时模型：按 agent 名设置/清除模型档案绑定（agents_registry.config.model_profile）。
	r.GET("/agents/:name/model", s.getAgentModel)
	r.PUT("/agents/:name/model", s.setAgentModel)

	r.GET("/evolve-tasks", s.listEvolveTasks)
	r.GET("/evolve-tasks/:id", s.getEvolveTask)

	// P4-1（ai-flow 两阶段确认审计双写）：外部 agent（actor=ai-flow）写入审计留痕；
	// on_behalf_of（代表执行的用户）落 detail，可按 detail.on_behalf_of 查到记录。
	// audit_logs 只增不删：本入口仅 INSERT，无任何查询/删除语义在此处。
	// ⚠️ 鉴权红线：写入与查询均挂 mws（requireAuth），未登录 401；写入内容
	// 不信任请求体自报（见 appendAuditLog：actor 固定服务身份、action 白名单、
	// on_behalf_of 以 JWT sub 为准）。
	auditGroup := r.Group("/audit-logs", mws...)
	auditGroup.POST("", s.appendAuditLog)

	// P4-1 验收配套：按 on_behalf_of 查审计记录（只读，同组鉴权）；非 admin
	// 在 handler 内被强制限定为 detail.on_behalf_of = JWT sub（见 listAuditLogs）。
	auditGroup.GET("", s.listAuditLogs)
}
