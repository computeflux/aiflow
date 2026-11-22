package approval

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
)

// routes_evolve.go：进化单查询路由（从 routes.go 拆出，纯机械搬迁）。

func (s *Store) listEvolveTasks(ctx *gin.Context) {
	status := ctx.Query("status")
	limit, _ := strconv.Atoi(ctx.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var rows []db.EvolveTask
	q := s.db.WithContext(ctx).Order("id DESC").Limit(limit)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Select("id", "task_no", "source", "target_name",
		"workdir", "status", "approval_id", "created_at", "updated_at").
		Find(&rows).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"tasks": rows})
}

func (s *Store) getEvolveTask(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	var t db.EvolveTask
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&t).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "evolve task not found"})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"task": t})
}
