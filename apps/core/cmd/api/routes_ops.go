package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/deploy"
	"github.com/wetee/ai-workflow/internal/ops"
)

// registerOpsRoutes 运维情报域：统一错误日志（error_logs 查询/统计/解决/删除）+
// S6 运维报告（docs/plan/S6-运维情报.md §3.1/§3.2）。
// 只读现有数据 + 只写 ops_reports / board 卡；建议为 high 风险时不生成自动任务卡（红线）。
// 逻辑自 main() 原样搬迁（批次 A 重构，行为无变更）。
func registerOpsRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database, opsStore := deps.database, deps.opsStore

	// 统一错误日志（各系统写入 error_logs 表，见 internal/otel/errorsink.go）
	r.GET("/errors", requireAuth(), func(ctx *gin.Context) {
		system := ctx.Query("system")
		level := ctx.Query("level")
		resolved := ctx.Query("resolved") // "" / "true" / "false"
		q := ctx.Query("q")
		page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
		pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
		if page < 1 {
			page = 1
		}
		if pageSize < 1 || pageSize > 100 {
			pageSize = 20
		}

		query := database.Model(&db.ErrorLog{})
		if system != "" {
			query = query.Where("system = ?", system)
		}
		if level != "" {
			query = query.Where("level = ?", level)
		}
		if resolved == "true" {
			query = query.Where("resolved = ?", true)
		} else if resolved == "false" {
			query = query.Where("resolved = ?", false)
		}
		if q != "" && len(q) >= 2 {
			// 只搜 message/source（context 是 LONGTEXT 全表扫描代价极高，workflow_id 无检索意义），
			// 且要求至少 2 字节，避免输入 1 个字符就触发全表 LIKE 扫描
			like := "%" + q + "%"
			query = query.Where("message LIKE ? OR source LIKE ?", like, like)
		}
		var total int64
		query.Count(&total)
		var items []db.ErrorLog
		query.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items)
		ctx.JSON(http.StatusOK, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
	})
	r.GET("/errors/stats", requireAuth(), func(ctx *gin.Context) {
		var total, today, unresolved int64
		database.Model(&db.ErrorLog{}).Count(&total)
		database.Model(&db.ErrorLog{}).Where("resolved = ?", false).Count(&unresolved)
		now := time.Now()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		database.Model(&db.ErrorLog{}).Where("created_at >= ?", todayStart).Count(&today)

		type GroupCount struct {
			Key   string `json:"key"`
			Count int64  `json:"count"`
		}
		var bySystem, byLevel []GroupCount
		// 注意：system 是 MySQL 8.0.17+ 保留字，必须加反引号，否则 GROUP BY 查询 1064 报错
		database.Model(&db.ErrorLog{}).Select("`system` as `key`, count(*) as `count`").Group("`system`").Order("`count` desc").Scan(&bySystem)
		database.Model(&db.ErrorLog{}).Select("`level` as `key`, count(*) as `count`").Group("`level`").Order("`count` desc").Scan(&byLevel)
		// GORM 在无结果时可能留下 nil slice，JSON 会序列化成 null（前端 .length/.map 会崩），补成空切片
		if bySystem == nil {
			bySystem = []GroupCount{}
		}
		if byLevel == nil {
			byLevel = []GroupCount{}
		}

		ctx.JSON(http.StatusOK, gin.H{"total": total, "today": today, "unresolved": unresolved, "by_system": bySystem, "by_level": byLevel})
	})
	r.POST("/errors/:id/resolve", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		now := time.Now()
		res := database.Model(&db.ErrorLog{}).Where("id = ?", id).Updates(map[string]any{"resolved": true, "resolved_at": &now})
		if res.RowsAffected == 0 {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.POST("/errors/resolve-all", requireAuth(), func(ctx *gin.Context) {
		now := time.Now()
		res := database.Model(&db.ErrorLog{}).Where("resolved = ?", false).Updates(map[string]any{"resolved": true, "resolved_at": &now})
		ctx.JSON(http.StatusOK, gin.H{"updated": res.RowsAffected})
	})
	r.DELETE("/errors/:id", requireRole("admin"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Delete(&db.ErrorLog{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	r.GET("/ops-reports", requireAuth(), func(ctx *gin.Context) {
		rtype := ctx.Query("type")
		items, err := opsStore.List(ctx, rtype)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if items == nil {
			items = []ops.OpsReport{}
		}
		ctx.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
	})
	r.GET("/ops-reports/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		rep, err := opsStore.Get(ctx, uint(id))
		if err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"report": rep})
	})
	r.POST("/ops-reports/suggest", requireAuth(), func(ctx *gin.Context) {
		// 按需生成升级建议（api 容器未挂载仓库 → git/TODO 降级为空；主要输入是 7 天错误/技能数据）。
		rep, err := ops.GenerateSuggestions(ctx, opsStore, database, deps.llm, deps.temporal, deps.cfg.TemporalNamespace, "")
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"report": rep})
	})
	r.POST("/ops-reports/:id/suggestions/:idx/card", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		idx, _ := strconv.Atoi(ctx.Param("idx"))
		var input struct {
			ListID   *uint  `json:"list_id"`
			Assignee string `json:"assignee"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil || input.ListID == nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "list_id is required"})
			return
		}
		rep, err := opsStore.Get(ctx, uint(id))
		if err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		var sm ops.SuggestionMetrics
		if err := json.Unmarshal([]byte(rep.Metrics), &sm); err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "no suggestions in report"})
			return
		}
		if idx < 0 || idx >= len(sm.Suggestions) {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "suggestion index out of range"})
			return
		}
		sug := sm.Suggestions[idx]
		// 红线：high 风险（manual_pr_only）或无卡模板 → 不生成自动任务卡
		if sug.ManualPROnly || sug.SuggestedCard == nil || sug.SuggestedCard.Title == "" {
			ctx.JSON(http.StatusForbidden, gin.H{"error": "该建议为高风险（仅人工 PR 建议单），不生成自动任务卡"})
			return
		}
		priority := sug.SuggestedCard.Priority
		if priority == "" {
			priority = "medium"
		}
		checklistJSON := "[]"
		if len(sug.SuggestedCard.Checklist) > 0 {
			type item struct {
				Text string `json:"text"`
				Done bool   `json:"done"`
			}
			items := make([]item, 0, len(sug.SuggestedCard.Checklist))
			for _, t := range sug.SuggestedCard.Checklist {
				items = append(items, item{Text: t, Done: false})
			}
			if b, err := json.Marshal(items); err == nil {
				checklistJSON = string(b)
			}
		}
		card := db.BoardCard{
			ListID:      *input.ListID,
			Title:       sug.SuggestedCard.Title,
			Description: sug.SuggestedCard.Description,
			Priority:    priority,
			Checklist:   checklistJSON,
			Source:      "ops_suggestion",
			Assignee:    input.Assignee,
			Labels:      "ops",
		}
		if err := database.Create(&card).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusCreated, card)
	})

	// 定期清理过期错误日志，防止 error_logs 无限膨胀
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			res := database.Where("created_at < ?", time.Now().Add(-30*24*time.Hour)).Delete(&db.ErrorLog{})
			if res.Error != nil {
				log.Printf("[errors] cleanup failed: %v", res.Error)
			} else if res.RowsAffected > 0 {
				log.Printf("[errors] cleaned %d old error logs", res.RowsAffected)
			}
		}
	}()
}

// registerServiceOpsRoutes 注册运维中心「运行服务总览」REST 端点（wi-1792321102）。
// 读操作需登录；写操作（重启/停止/启动/回滚/改参）需 admin 角色，全部写 audit_logs。
// serviceOps 为 nil（Docker socket 不可用）时端点返回 503 降级提示。
// 错误映射：ErrServiceNotFound → 404、ErrNotManaged → 403、其余（docker 内部失败等）→ 500。
func registerServiceOpsRoutes(r *gin.Engine, so *deploy.ServiceOps, database *db.DB, requireAuth func() gin.HandlerFunc, requireRole func(roles ...string) gin.HandlerFunc) {
	notAvailable := func(ctx *gin.Context) {
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "运行服务总览未启用（Docker socket / REPO_ROOT 不可用）"})
	}
	requireAdmin := requireRole("admin")
	// 写操作错误统一映射：404（服务不存在）/ 403（不纳管）/ 400（参数非法）/ 409（自操作冲突）/ 500（内部失败）。
	// 500 走 serverError：对外只回 "internal error"，细节写 error_logs（避免泄露 docker daemon 原始报错，且进运维错误看板）。
	writeErr := func(ctx *gin.Context, err error) {
		switch {
		case errors.Is(err, deploy.ErrServiceNotFound):
			ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, deploy.ErrNotManaged):
			ctx.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		case errors.Is(err, deploy.ErrInvalidRequest):
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, deploy.ErrSelfOperation):
			ctx.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			ctx.JSON(http.StatusInternalServerError, serverError(database, "serviceops", err))
		}
	}

	r.GET("/services", requireAuth(), func(ctx *gin.Context) {
		if so == nil {
			notAvailable(ctx)
			return
		}
		items, err := so.ListServices(ctx)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, serverError(database, "serviceops", err))
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"services": items})
	})
	r.GET("/services/:name", requireAuth(), func(ctx *gin.Context) {
		if so == nil {
			notAvailable(ctx)
			return
		}
		tail, _ := strconv.Atoi(ctx.DefaultQuery("tail", "200"))
		if tail <= 0 || tail > 2000 {
			tail = 2000 // 上界钳制：防极大 tail 导致日志流读入内存的带宽/内存放大
		}
		detail, err := so.GetServiceDetail(ctx, ctx.Param("name"), tail)
		if err != nil {
			if errors.Is(err, deploy.ErrServiceNotFound) {
				ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}
			ctx.JSON(http.StatusInternalServerError, serverError(database, "serviceops", err))
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"service": detail})
	})
	r.POST("/services/:name/restart", requireAdmin, func(ctx *gin.Context) {
		if so == nil {
			notAvailable(ctx)
			return
		}
		if err := so.RestartService(ctx, ctx.Param("name"), authn.UserID(ctx)); err != nil {
			writeErr(ctx, err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "action": "restart"})
	})
	r.POST("/services/:name/stop", requireAdmin, func(ctx *gin.Context) {
		if so == nil {
			notAvailable(ctx)
			return
		}
		if err := so.StopService(ctx, ctx.Param("name"), authn.UserID(ctx)); err != nil {
			writeErr(ctx, err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "action": "stop"})
	})
	r.POST("/services/:name/start", requireAdmin, func(ctx *gin.Context) {
		if so == nil {
			notAvailable(ctx)
			return
		}
		if err := so.StartService(ctx, ctx.Param("name"), authn.UserID(ctx)); err != nil {
			writeErr(ctx, err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "action": "start"})
	})
	r.POST("/services/:name/rollback", requireAdmin, func(ctx *gin.Context) {
		if so == nil {
			notAvailable(ctx)
			return
		}
		var body struct {
			Image string `json:"image"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Image) == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "image is required（回滚目标镜像）"})
			return
		}
		body.Image = strings.TrimSpace(body.Image) // 统一用 trim 后的值（校验与传参一致，避免带空格镜像名导致误导性错误）
		if err := so.RollbackService(ctx, ctx.Param("name"), body.Image, authn.UserID(ctx)); err != nil {
			writeErr(ctx, err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "action": "rollback", "image": body.Image})
	})
	r.POST("/services/:name/params", requireAdmin, func(ctx *gin.Context) {
		if so == nil {
			notAvailable(ctx)
			return
		}
		var upd deploy.ParamUpdate
		if err := ctx.ShouldBindJSON(&upd); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := so.UpdateServiceParams(ctx, ctx.Param("name"), upd, authn.UserID(ctx)); err != nil {
			writeErr(ctx, err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "action": "update_params"})
	})
}
