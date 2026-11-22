package approval

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/db"
)

// audit_ingest.go：P4-1 审计双写入口与审计查询（从 routes.go 拆出，纯机械搬迁）。

// appendAuditLog 是 P4-1 审计双写入口（ai-flow confirm 后调用）。
//
// 身份与内容红线（audit_logs 只增不删，写入内容绝不信任请求体自报）：
//   - actor 固定为服务身份 "ai-flow"；请求体传了其它 actor → 403；
//   - on_behalf_of 以 JWT sub（authn.UserID，requireAuth 中间件注入）为准；
//     请求体自报的 on_behalf_of 与 JWT sub 不一致 → 403（只能代表自己）；
//     未部署 auth（无 JWT 环境）时才回退使用自报值；
//   - action 白名单：仅允许 picoclaw_write_*（缺省 picoclaw_write_confirm），
//     其它值（如 approval_approved）→ 403，防登录用户伪造审批/CD 事件流。
//
// 落库映射：actor="ai-flow"，action（缺省 picoclaw_write_confirm），
// target=tool，detail={on_behalf_of, params_hash, tool, result}。
func (s *Store) appendAuditLog(ctx *gin.Context) {
	var req struct {
		Actor      string `json:"actor"`
		OnBehalfOf string `json:"on_behalf_of"`
		Tool       string `json:"tool"`
		ParamsHash string `json:"params_hash"`
		Result     string `json:"result"`
		Action     string `json:"action"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid audit log payload: " + err.Error()})
		return
	}
	if req.Tool == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "tool is required"})
		return
	}
	// actor 固定服务身份：本入口是 ai-flow 专用通道，其它 actor（admin /
	// expiry / cd ...）的审计由服务端内部 Log 写入，不接受外部自报。
	const auditActor = "ai-flow"
	if req.Actor != "" && req.Actor != auditActor {
		ctx.JSON(http.StatusForbidden, gin.H{"error": "actor must be ai-flow"})
		return
	}
	// 白名单：本入口只接受 ai-flow 确认链的 action（picoclaw_write_*）；
	// result 是结果标签（如 confirmed_executed），不是 action，不能充当
	// action 绕过白名单伪造审批/CD 事件流。
	if req.Action != "" && !strings.HasPrefix(req.Action, "picoclaw_write_") {
		ctx.JSON(http.StatusForbidden, gin.H{"error": "action must be a picoclaw_write_* action"})
		return
	}
	action := req.Action
	if action == "" {
		action = "picoclaw_write_confirm"
	}
	// on_behalf_of 以 JWT sub 为准（requireAuth 注入）；自报与 sub 不一致 →
	// 拒绝（不能代表其它用户）。无 auth 环境（sub 为空）回退自报值。
	jwtUser := authn.UserID(ctx)
	onBehalfOf := req.OnBehalfOf
	if jwtUser != "" {
		if onBehalfOf != "" && onBehalfOf != jwtUser {
			ctx.JSON(http.StatusForbidden, gin.H{"error": "on_behalf_of must equal the authenticated user"})
			return
		}
		onBehalfOf = jwtUser
	}
	detail := map[string]any{
		"on_behalf_of": onBehalfOf,
		"tool":         req.Tool,
		"params_hash":  req.ParamsHash,
		"result":       req.Result,
	}
	if err := s.Log(ctx, auditActor, action, req.Tool, detail); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "write audit_log: " + err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"ok": true})
}

// listAuditLogs 按条件只读查 audit_logs（P4-1：验收「可按 on_behalf_of 查到
// 记录」；actor / target 可选过滤，limit 默认 50，上限 500，id 降序）。
//
// 敏感读收口：非 admin（JWT roles 不含 admin）强制只看自己 —— 查询条件
// 被限制为 detail.on_behalf_of = JWT sub，请求里指定别人的 on_behalf_of
// 直接 403；admin 无限制。无 auth 环境（jwtUser 与 roles 均空）不附加
// 额外限制（仅保留 mws 鉴权，与未部署 auth 的部署形态一致）。
func (s *Store) listAuditLogs(ctx *gin.Context) {
	limit, _ := strconv.Atoi(ctx.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := s.db.WithContext(ctx).Model(&db.AuditLog{}).Order("id DESC").Limit(limit)

	jwtUser := authn.UserID(ctx)
	isAdmin := false
	for _, r := range authn.Roles(ctx) {
		if r == "admin" {
			isAdmin = true
			break
		}
	}
	if jwtUser != "" && !isAdmin {
		// 非 admin：强制限定自己的记录。
		if v := ctx.Query("on_behalf_of"); v != "" && v != jwtUser {
			ctx.JSON(http.StatusForbidden, gin.H{"error": "non-admin callers may only query their own on_behalf_of"})
			return
		}
		q = q.Where("JSON_EXTRACT(detail, '$.on_behalf_of') = ?", jwtUser)
	} else if v := ctx.Query("on_behalf_of"); v != "" {
		// detail 列是 JSON：JSON_EXTRACT 在 MySQL（原生 JSON）与 sqlite/JSON1
		// （detail 存 TEXT）下语义一致，直接取 on_behalf_of 字段精确匹配。
		q = q.Where("JSON_EXTRACT(detail, '$.on_behalf_of') = ?", v)
	}
	if v := ctx.Query("actor"); v != "" {
		q = q.Where("actor = ?", v)
	}
	if v := ctx.Query("target"); v != "" {
		q = q.Where("target = ?", v)
	}
	var rows []db.AuditLog
	if err := q.Find(&rows).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"logs": rows, "total": len(rows)})
}
