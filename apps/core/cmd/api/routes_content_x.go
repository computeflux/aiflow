package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
)

// registerContentXRoutes 内容生产域·X 点赞账号与订阅目标管理（x_accounts / follow_targets，
// pyreplay worker 侧数据）。逻辑自 registerContentRoutes 原样搬迁（批次 F 拆分，行为无变更）。
func registerContentXRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database

	maskAccount := func(a db.XAccount) gin.H {
		return gin.H{
			"id": a.ID, "machine_id": a.MachineID, "username": a.Username,
			"auth_token": maskSecret(a.AuthToken), "ct0": maskSecret(a.Ct0),
			"enabled": a.Enabled, "daily_like_limit": a.DailyLikeLimit,
			"cooldown_minutes": a.CooldownMinutes, "used_today": a.UsedToday,
			"last_used_at": a.LastUsedAt, "created_at": a.CreatedAt, "updated_at": a.UpdatedAt,
		}
	}

	r.GET("/x-accounts", requireAuth(), func(ctx *gin.Context) {
		var accounts []db.XAccount
		query := database.Order("id desc")
		if machine := ctx.Query("machine_id"); machine != "" {
			query = query.Where("machine_id = ?", machine)
		}
		query.Find(&accounts)
		out := make([]gin.H, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, maskAccount(a))
		}
		ctx.JSON(http.StatusOK, gin.H{"accounts": out})
	})

	r.POST("/x-accounts", requireRole("admin"), func(ctx *gin.Context) {
		var input struct {
			MachineID       string `json:"machine_id"`
			Username        string `json:"username"`
			AuthToken       string `json:"auth_token"`
			Ct0             string `json:"ct0"`
			Enabled         *bool  `json:"enabled"`
			DailyLikeLimit  int    `json:"daily_like_limit"`
			CooldownMinutes int    `json:"cooldown_minutes"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.AuthToken == "" || input.Ct0 == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "auth_token and ct0 are required"})
			return
		}
		account := db.XAccount{
			MachineID: input.MachineID, Username: input.Username, AuthToken: input.AuthToken, Ct0: input.Ct0,
			DailyLikeLimit: input.DailyLikeLimit, CooldownMinutes: input.CooldownMinutes,
		}
		if input.Enabled != nil {
			account.Enabled = *input.Enabled
		}
		if account.DailyLikeLimit == 0 {
			account.DailyLikeLimit = 50
		}
		if account.CooldownMinutes == 0 {
			account.CooldownMinutes = 10
		}
		database.Create(&account)
		ctx.JSON(http.StatusCreated, maskAccount(account))
	})

	r.PUT("/x-accounts/:id", requireRole("admin"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var account db.XAccount
		if err := database.First(&account, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
			return
		}
		var input struct {
			MachineID       *string `json:"machine_id"`
			Username        *string `json:"username"`
			AuthToken       *string `json:"auth_token"`
			Ct0             *string `json:"ct0"`
			Enabled         *bool   `json:"enabled"`
			DailyLikeLimit  *int    `json:"daily_like_limit"`
			CooldownMinutes *int    `json:"cooldown_minutes"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := map[string]any{"updated_at": time.Now()}
		if input.MachineID != nil {
			updates["machine_id"] = *input.MachineID
		}
		if input.Username != nil {
			updates["username"] = *input.Username
		}
		if input.AuthToken != nil && *input.AuthToken != "" {
			updates["auth_token"] = *input.AuthToken
		}
		if input.Ct0 != nil && *input.Ct0 != "" {
			updates["ct0"] = *input.Ct0
		}
		if input.Enabled != nil {
			updates["enabled"] = *input.Enabled
		}
		if input.DailyLikeLimit != nil {
			updates["daily_like_limit"] = *input.DailyLikeLimit
		}
		if input.CooldownMinutes != nil {
			updates["cooldown_minutes"] = *input.CooldownMinutes
		}
		database.Model(&account).Updates(updates)
		database.First(&account, id)
		ctx.JSON(http.StatusOK, maskAccount(account))
	})

	r.DELETE("/x-accounts/:id", requireRole("admin"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Delete(&db.XAccount{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	// 订阅目标管理（pyreplay worker 的 follow_targets 表）
	r.GET("/follow-targets", requireAuth(), func(ctx *gin.Context) {
		var targets []db.FollowTarget
		query := database.Order("id desc")
		if machine := ctx.Query("machine_id"); machine != "" {
			query = query.Where("machine_id = ?", machine)
		}
		query.Find(&targets)
		ctx.JSON(http.StatusOK, gin.H{"targets": targets})
	})

	r.POST("/follow-targets", requireRole("admin"), func(ctx *gin.Context) {
		var input struct {
			MachineID           string `json:"machine_id"`
			Handle              string `json:"handle"`
			Enabled             *bool  `json:"enabled"`
			ScanIntervalMinutes int    `json:"scan_interval_minutes"`
			MaxLikePerScan      int    `json:"max_like_per_scan"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		input.Handle = strings.TrimPrefix(strings.TrimSpace(input.Handle), "@")
		if input.Handle == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "handle is required"})
			return
		}
		target := db.FollowTarget{
			MachineID: input.MachineID, Handle: input.Handle, ScanIntervalMinutes: input.ScanIntervalMinutes,
			MaxLikePerScan: input.MaxLikePerScan,
		}
		if input.Enabled != nil {
			target.Enabled = *input.Enabled
		}
		if target.ScanIntervalMinutes == 0 {
			target.ScanIntervalMinutes = 15
		}
		if target.MaxLikePerScan == 0 {
			target.MaxLikePerScan = 5
		}
		if err := database.Create(&target).Error; err != nil {
			ctx.JSON(http.StatusBadRequest, serverError(database, "follow-targets", err))
			return
		}
		ctx.JSON(http.StatusCreated, target)
	})

	r.PUT("/follow-targets/:id", requireRole("admin"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var target db.FollowTarget
		if err := database.First(&target, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "target not found"})
			return
		}
		var input struct {
			MachineID           *string `json:"machine_id"`
			Handle              *string `json:"handle"`
			Enabled             *bool   `json:"enabled"`
			ScanIntervalMinutes *int    `json:"scan_interval_minutes"`
			MaxLikePerScan      *int    `json:"max_like_per_scan"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := map[string]any{"updated_at": time.Now()}
		if input.MachineID != nil {
			updates["machine_id"] = *input.MachineID
		}
		if input.Handle != nil {
			updates["handle"] = strings.TrimPrefix(strings.TrimSpace(*input.Handle), "@")
		}
		if input.Enabled != nil {
			updates["enabled"] = *input.Enabled
		}
		if input.ScanIntervalMinutes != nil {
			updates["scan_interval_minutes"] = *input.ScanIntervalMinutes
		}
		if input.MaxLikePerScan != nil {
			updates["max_like_per_scan"] = *input.MaxLikePerScan
		}
		database.Model(&target).Updates(updates)
		database.First(&target, id)
		ctx.JSON(http.StatusOK, target)
	})

	r.DELETE("/follow-targets/:id", requireRole("admin"), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Delete(&db.FollowTarget{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})
}
