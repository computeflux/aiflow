package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workitem"
	"gorm.io/gorm"
)

// registerBoardCrudRoutes 看板·Board/List/Card/评论 CRUD。逻辑自 registerBoardRoutes 原样
// 搬迁（批次 F 拆分，行为无变更）。
func registerBoardCrudRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database
	workitemStore := deps.workitemStore
	apvStore := deps.apvStore

	// Board API
	r.GET("/boards", requireAuth(), func(ctx *gin.Context) {
		var boards []db.Board
		database.Order("updated_at desc").Find(&boards)
		ctx.JSON(http.StatusOK, gin.H{"boards": boards})
	})

	r.POST("/boards", requireAuth(), func(ctx *gin.Context) {
		var b db.Board
		if err := ctx.ShouldBindJSON(&b); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		database.Create(&b)
		for i, name := range []string{"Backlog", "Todo", "In Progress", "Review", "Done"} {
			database.Create(&db.BoardList{BoardID: b.ID, Name: name, Position: i})
		}
		ctx.JSON(http.StatusCreated, gin.H{"board": b})
	})

	r.GET("/boards/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var board db.Board
		if err := database.Preload("Lists", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).Preload("Lists.Cards", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).First(&board, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		// 实时投影：卡状态/列以关联 evolve_task 为准（不等 5min 镜像轮；只改响应不写库）
		applyLiveCardStatus(ctx, database, &board)
		// 显式前置依赖：未合入主分支的卡在响应里带上 blocked_by（前端展示「需 #xxx 完成后才开始」）
		applyBlockedBy(ctx, database, &board)
		ctx.JSON(http.StatusOK, board)
	})

	r.PUT("/boards/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var input struct {
			Name *string `json:"name"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.Name != nil {
			database.Model(&db.Board{}).Where("id = ?", id).Updates(map[string]any{"name": *input.Name, "updated_at": time.Now()})
		}
		ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.DELETE("/boards/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Where("board_id = ?", id).Delete(&db.BoardList{})
		database.Delete(&db.Board{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	r.POST("/boards/:id/lists", requireAuth(), func(ctx *gin.Context) {
		boardID, _ := strconv.Atoi(ctx.Param("id"))
		var l db.BoardList
		if err := ctx.ShouldBindJSON(&l); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		l.BoardID = uint(boardID)
		database.Create(&l)
		ctx.JSON(http.StatusCreated, l)
	})

	r.PUT("/lists/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var input struct {
			Name     *string `json:"name"`
			Position *int    `json:"position"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := map[string]any{}
		if input.Name != nil {
			updates["name"] = *input.Name
		}
		if input.Position != nil {
			updates["position"] = *input.Position
		}
		database.Model(&db.BoardList{}).Where("id = ?", id).Updates(updates)
		ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.DELETE("/lists/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Where("list_id = ?", id).Delete(&db.BoardCard{})
		database.Delete(&db.BoardList{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	r.POST("/cards", requireAuth(), func(ctx *gin.Context) {
		var c db.BoardCard
		if err := ctx.ShouldBindJSON(&c); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if c.Source == "" {
			c.Source = "manual"
		}
		database.Create(&c)
		ctx.JSON(http.StatusCreated, c)
	})

	r.PUT("/cards/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var card db.BoardCard
		if err := database.First(&card, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		var input struct {
			Title       *string    `json:"title"`
			Description *string    `json:"description"`
			ListID      *uint      `json:"list_id"`
			Position    *int       `json:"position"`
			Assignee    *string    `json:"assignee"`
			Labels      *string    `json:"labels"`
			TargetName  *string    `json:"target_name"` // S9.8 §7.10：目标项目/技能名（专门字段）
			DueDate     *time.Time `json:"due_date"`
			Priority    *string    `json:"priority"`
			CoverColor  *string    `json:"cover_color"`
			Checklist   *string    `json:"checklist"`
			DependsOn   *string    `json:"depends_on"` // 显式前置卡（JSON 数组/逗号分隔），未合入主分支前不可开始
			Status      *string    `json:"status"`     // S9：todo / ready_for_evolve（交给工作流）/ done
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// 启动前依赖检查：拖到 In Progress（立即执行）时判断前置是否就绪。
		// 显式依赖（card.depends_on）→ 确定性判定，前置改动**未合入主分支**即 409；
		// 没填显式依赖 → 退回 LLM 推断（fail-open，不让模型故障把看板卡死）。
		if input.ListID != nil && *input.ListID != card.ListID {
			var destList db.BoardList
			if err := database.DB.WithContext(ctx).First(&destList, *input.ListID).Error; err == nil && destList.Name == "In Progress" {
				var curList db.BoardList
				if err := database.DB.WithContext(ctx).First(&curList, card.ListID).Error; err == nil {
					var boardCards []db.BoardCard
					if err := database.DB.WithContext(ctx).
						Where("list_id IN (?)", database.DB.Model(&db.BoardList{}).Select("id").Where("board_id = ?", curList.BoardID)).
						Find(&boardCards).Error; err == nil {
						if blockers, reason := workitem.CheckDependenciesWithStore(ctx, workitemStore, deps.llm, boardCards, card); len(blockers) > 0 {
							actor := authn.UserID(ctx)
							if strings.TrimSpace(actor) == "" {
								actor = "board"
							}
							apvStore.Log(ctx, actor, "card_start_blocked", fmt.Sprintf("card-%d", card.ID), map[string]any{
								"blocked_by": blockers, "reason": reason,
							})
							ctx.JSON(http.StatusConflict, gin.H{
								"error":      reason,
								"blocked_by": blockers,
							})
							return
						}
					}
				}
			}
		}
		// ⚠ GORM 的 Model(&card).Updates(map) 会把新值写回内存结构体 → 判断"是否换列"必须先记住旧值再比较
		oldListID := card.ListID
		updates := map[string]any{"updated_at": time.Now()}
		if input.Title != nil {
			updates["title"] = *input.Title
		}
		if input.Description != nil {
			updates["description"] = *input.Description
		}
		if input.ListID != nil {
			updates["list_id"] = *input.ListID
		}
		if input.Position != nil {
			updates["position"] = *input.Position
		}
		if input.Assignee != nil {
			updates["assignee"] = *input.Assignee
		}
		if input.Labels != nil {
			updates["labels"] = *input.Labels
		}
		if input.TargetName != nil {
			updates["target_name"] = *input.TargetName
		}
		if input.DueDate != nil {
			updates["due_date"] = input.DueDate
		}
		if input.Priority != nil {
			updates["priority"] = *input.Priority
		}
		if input.CoverColor != nil {
			updates["cover_color"] = *input.CoverColor
		}
		if input.Checklist != nil {
			updates["checklist"] = *input.Checklist
		}
		if input.DependsOn != nil {
			updates["depends_on"] = *input.DependsOn
		}
		if input.Status != nil {
			updates["status"] = *input.Status
		}
		database.Model(&card).Updates(updates)
		// S9 双向同步（§7.6 看板 → 工作流）：卡已关联工作项时，用户改卡状态 = 对工作项下命令
		//（如手动标 done = 人工验收 shipped）。非法迁移静默忽略，
		// 下轮镜像同步（work-dispatch）会把卡状态重新对齐为工作项状态。
		// "card-<id>"（分诊 Bug 卡的卡关联）不是工作项号 → 不操作。
		if input.Status != nil && card.WorkItemNo != "" && !workitem.IsCardRef(card.WorkItemNo) {
			if wiErr := syncCardStatusToWorkItem(ctx, workitemStore, card.WorkItemNo, *input.Status); wiErr != nil {
				log.Printf("WARN: card %d → work item %s status sync: %v", id, card.WorkItemNo, wiErr)
			}
		}
		// S9 看板为核心：用户拖卡换列 = 对关联工作项下命令（拖 Done=人工验收；拖回 Backlog/Todo=收回重开）
		if input.ListID != nil && *input.ListID != oldListID && card.WorkItemNo != "" && !workitem.IsCardRef(card.WorkItemNo) {
			if wiErr := syncCardColumnToWorkItem(ctx, database, workitemStore, card.WorkItemNo, *input.ListID); wiErr != nil {
				log.Printf("WARN: card %d column %d → work item %s sync: %v", id, *input.ListID, card.WorkItemNo, wiErr)
			}
		}
		// S9 看板为核心（2026-10-28 列语义对齐）：列 = 对工作流下的命令
		//   - 拖到 **Todo**（待执行）= 交给工作流：建/重开工作项 → queued，等调度器领取；
		//   - 拖到 **In Progress**（执行中）= 立即执行：入队 + 立即领取（容量满则留在 Todo）；
		//   - Backlog / Done 由 syncCardColumnToWorkItem 翻译；Review 由流水线自动进入。
		if input.ListID != nil && *input.ListID != oldListID {
			var destList db.BoardList
			if database.DB.WithContext(ctx).First(&destList, *input.ListID).Error == nil &&
				(destList.Name == "Todo" || destList.Name == "In Progress") {
				no, hoErr := workitem.HandOffCard(ctx, workitemStore, workitem.DefaultWeights(), &card)
				if hoErr != nil {
					log.Printf("WARN: card %d hand-off (%s) → work item: %v", id, destList.Name, hoErr)
				} else {
					targetNo := no
					if targetNo == "" && card.WorkItemNo != "" && !workitem.IsCardRef(card.WorkItemNo) {
						targetNo = card.WorkItemNo // 已在流水线（queued）→ 直接领取
					}
					if targetNo != "" {
						log.Printf("card %d handed off to workflow via %s: work item %s", id, destList.Name, targetNo)
						if destList.Name == "In Progress" {
							// 仅 queued 可领取；in_progress 已是在执行，跳过（不报错）
							var it db.WorkItem
							if err := database.DB.WithContext(ctx).Where("no = ?", targetNo).First(&it).Error; err == nil && it.Status == workitem.StatusQueued {
								if _, perr := workitem.PickItem(ctx, workitemStore, targetNo); perr != nil {
									log.Printf("card %d 立即执行未成功（并发满，等调度器）：%v", id, perr)
								}
							}
						}
						// 即时反馈：状态 + 列对齐到新工作项状态（不等 5min 镜像轮）
						if sErr := workitem.SyncOneCardStatus(ctx, workitemStore, card.ID, targetNo); sErr != nil {
							log.Printf("WARN: card %d immediate status sync: %v", id, sErr)
						}
					}
				}
			}
		}
		ctx.JSON(http.StatusOK, gin.H{"card": card})
	})

	r.DELETE("/cards/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Where("card_id = ?", id).Delete(&db.BoardComment{})
		database.Delete(&db.BoardCard{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	// Board comments
	r.GET("/cards/:id/comments", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var comments []db.BoardComment
		database.Where("card_id = ?", id).Order("created_at asc").Find(&comments)
		ctx.JSON(http.StatusOK, gin.H{"comments": comments})
	})

	r.POST("/cards/:id/comments", requireAuth(), func(ctx *gin.Context) {
		cardID, _ := strconv.Atoi(ctx.Param("id"))
		var c db.BoardComment
		if err := ctx.ShouldBindJSON(&c); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.CardID = uint(cardID)
		database.Create(&c)
		ctx.JSON(http.StatusCreated, c)
	})
}
