package handler

import (
	"net/http"

	"github.com/computeflux/auth/middleware"
	"github.com/gin-gonic/gin"
)

// ListRoles GET /roles
func (h *Handler) ListRoles(c *gin.Context) {
	roles, err := h.RBAC.ListRoles()
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": roles})
}

type createRoleRequest struct {
	ID          string   `json:"id" binding:"required"`
	Name        string   `json:"name" binding:"required"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// CreateRole POST /roles
func (h *Handler) CreateRole(c *gin.Context) {
	var req createRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id and name required"})
		return
	}
	if err := h.RBAC.CreateRole(req.ID, req.Name, req.Description, req.Permissions, middleware.AdminBy(c)); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type updateRoleRequest struct {
	Permissions []string `json:"permissions" binding:"required"`
}

// UpdateRole PUT /roles/{id}（内置角色只可加不可删基础权限）
func (h *Handler) UpdateRole(c *gin.Context) {
	var req updateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "permissions required"})
		return
	}
	if err := h.RBAC.UpdateRole(c.Param("id"), req.Permissions, middleware.AdminBy(c)); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteRole DELETE /roles/{id}
func (h *Handler) DeleteRole(c *gin.Context) {
	if err := h.RBAC.DeleteRole(c.Param("id"), middleware.AdminBy(c)); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ListPermissions GET /permissions（?app= 过滤）
func (h *Handler) ListPermissions(c *gin.Context) {
	perms, err := h.RBAC.ListPermissions(c.Query("app"))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"permissions": perms})
}

type registerPermissionRequest struct {
	ID          string `json:"id" binding:"required"`
	App         string `json:"app"`
	Description string `json:"description"`
}

// RegisterPermission POST /permissions（幂等 upsert）
func (h *Handler) RegisterPermission(c *gin.Context) {
	var req registerPermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	if err := h.RBAC.RegisterPermission(req.ID, req.App, req.Description); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Authorize POST /authorize（实时权限判定；Bearer 透传或 X-Admin-Token）
func (h *Handler) Authorize(c *gin.Context) {
	var req struct {
		UserID     string `json:"user_id" binding:"required"`
		Permission string `json:"permission" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id and permission required"})
		return
	}
	allowed, reason := h.RBAC.HasPermission(req.UserID, req.Permission)
	if !allowed {
		status := http.StatusForbidden
		if reason == "permission_denied" {
			status = http.StatusOK
		}
		c.JSON(status, gin.H{"allowed": false, "reason": reason})
		return
	}
	c.JSON(http.StatusOK, gin.H{"allowed": true})
}
