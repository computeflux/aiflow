package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/computeflux/auth/model"
	"github.com/gin-gonic/gin"
)

// ListAudit GET /audit（?user=&action=&from=&to=&page=&size=）
func (h *Handler) ListAudit(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	q := h.DB.Model(&model.AuditLog{})
	if u := c.Query("user"); u != "" {
		q = q.Where("user_id = ?", u)
	}
	if a := c.Query("action"); a != "" {
		q = q.Where("action = ?", a)
	}
	if from := c.Query("from"); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			q = q.Where("created_at >= ?", t)
		}
	}
	if to := c.Query("to"); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			q = q.Where("created_at <= ?", t)
		}
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		respondErr(c, err)
		return
	}
	var items []model.AuditLog
	if err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&items).Error; err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "items": items})
}
