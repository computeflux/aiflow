package handler

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/computeflux/auth/middleware"
	"github.com/computeflux/auth/model"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// ListUsers GET /users（分页 + q 模糊）
func (h *Handler) ListUsers(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	q := h.DB.Model(&model.User{})
	if kw := strings.TrimSpace(c.Query("q")); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("email LIKE ? OR name LIKE ? OR wallet_addr LIKE ?", like, like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		respondErr(c, err)
		return
	}
	var users []model.User
	if err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&users).Error; err != nil {
		respondErr(c, err)
		return
	}
	// 批量查角色（管理列表需要展示角色，如 admin）
	rolesByUser := map[string][]string{}
	if len(users) > 0 {
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		var roleRows []model.UserRole
		_ = h.DB.Where("user_id IN ?", ids).Find(&roleRows)
		for _, r := range roleRows {
			rolesByUser[r.UserID] = append(rolesByUser[r.UserID], r.RoleID)
		}
	}
	items := make([]gin.H, 0, len(users))
	for _, u := range users {
		roles := rolesByUser[u.ID]
		if roles == nil {
			roles = []string{}
		}
		items = append(items, gin.H{
			"id": u.ID, "email": u.Email, "name": u.Name, "wallet_addr": u.WalletAddr,
			"status": statusStr(u.Status), "roles": roles, "created_at": u.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "page": page, "size": size, "items": items})
}

var walletAddrRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

type createUserRequest struct {
	Wallet string   `json:"wallet" binding:"required"`
	Roles  []string `json:"roles"` // 额外角色(如 ["admin"]); 空 = 只授基础 user 角色
}

// CreateUser POST /users（按钱包地址手动添加用户，典型用途: 预创建管理员;
// 创建后该地址首次钱包登录即自动合并到本用户，无需重复建号）
func (h *Handler) CreateUser(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "wallet required"})
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Wallet))
	if !walletAddrRe.MatchString(addr) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid wallet address (0x + 40 hex)"})
		return
	}
	// 同一钱包地址全系统一个身份（与钱包登录 upsert 的 Subject 一致）
	var existing model.User
	if err := h.DB.Where("wallet_addr = ?", addr).First(&existing).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "user with this wallet already exists"})
		return
	} else if err != gorm.ErrRecordNotFound {
		respondErr(c, err)
		return
	}
	// 去重角色 + 基础 user 角色必授
	grant := map[string]bool{"user": true}
	for _, r := range req.Roles {
		if r = strings.TrimSpace(r); r != "" {
			grant[r] = true
		}
	}
	grantList := make([]string, 0, len(grant))
	for r := range grant {
		grantList = append(grantList, r)
	}

	user := model.User{
		ID:         ulid.MustNew(ulid.Timestamp(time.Now()), ulid.DefaultEntropy()).String(),
		Idp:        "wallet",
		IdpSubject: addr,
		WalletAddr: addr,
		Status:     1,
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Create(&user).Error; e != nil {
			return e
		}
		for _, r := range grantList {
			var role model.Role
			if e := tx.Where("id = ?", r).First(&role).Error; e != nil {
				if e == gorm.ErrRecordNotFound {
					return model.NewErr(400, "role not found: "+r)
				}
				return e
			}
			var n int64
			if e := tx.Model(&model.UserRole{}).Where("user_id = ? AND role_id = ?", user.ID, r).Count(&n).Error; e != nil {
				return e
			}
			if n == 0 {
				if e := tx.Create(&model.UserRole{UserID: user.ID, RoleID: r, GrantedBy: middleware.AdminBy(c), GrantedAt: time.Now()}).Error; e != nil {
					return e
				}
			}
		}
		return nil
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	h.Audit.Log(middleware.AdminBy(c), "user_create",
		map[string]string{"id": user.ID, "wallet": addr, "roles": strings.Join(grantList, ",")}, "")
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": user.ID, "wallet": addr, "roles": grantList})
}

// GetUser GET /users/{id}（含角色、直授权限、会话列表）
func (h *Handler) GetUser(c *gin.Context) {
	user, roles, err := h.userDetail(c.Param("id"))
	if err != nil {
		respondErr(c, err)
		return
	}
	var ups []model.UserPermission
	_ = h.DB.Where("user_id = ?", user.ID).Find(&ups)
	direct := make([]model.DirectPermissionDTO, 0, len(ups))
	for _, up := range ups {
		direct = append(direct, model.DirectPermissionDTO{Permission: up.PermissionID, ExpiresAt: up.ExpiresAt})
	}
	var rts []model.RefreshToken
	_ = h.DB.Where("user_id = ? AND revoked_at IS NULL", user.ID).Find(&rts)
	sessions := make([]model.SessionDTO, 0, len(rts))
	for _, rt := range rts {
		sessions = append(sessions, model.SessionDTO{
			ID: rt.ID, Device: rt.Device, CreatedAt: rt.CreatedAt, ExpiresAt: rt.ExpiresAt,
		})
	}
	c.JSON(http.StatusOK, model.UserDetailResponse{
		ID: user.ID, Email: user.Email, Name: user.Name, Avatar: user.Avatar,
		WalletAddr: user.WalletAddr, Status: statusStr(user.Status),
		Roles: roles, DirectPermissions: direct, Sessions: sessions, CreatedAt: user.CreatedAt,
	})
}

// DisableUser POST /users/{id}/disable（禁用 + 立即吊销全部 refresh）
func (h *Handler) DisableUser(c *gin.Context) {
	id := c.Param("id")
	var user model.User
	if err := h.DB.First(&user, "id = ?", id).Error; err != nil {
		respondErr(c, model.NewErr(404, "user not found"))
		return
	}
	if err := h.DB.Model(&user).Update("status", 0).Error; err != nil {
		respondErr(c, err)
		return
	}
	h.Session.RevokeAll(id)
	h.Audit.Log(middleware.AdminBy(c), "user_disable", map[string]string{"user": id}, "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// EnableUser POST /users/{id}/enable
func (h *Handler) EnableUser(c *gin.Context) {
	id := c.Param("id")
	var user model.User
	if err := h.DB.First(&user, "id = ?", id).Error; err != nil {
		respondErr(c, model.NewErr(404, "user not found"))
		return
	}
	if err := h.DB.Model(&user).Update("status", 1).Error; err != nil {
		respondErr(c, err)
		return
	}
	h.Audit.Log(middleware.AdminBy(c), "user_enable", map[string]string{"user": id}, "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteUser DELETE /users/{id}（级联删角色/直授/会话；保留审计；禁止删除自己）
func (h *Handler) DeleteUser(c *gin.Context) {
	id := c.Param("id")
	if id == middleware.AdminBy(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete yourself"})
		return
	}
	var user model.User
	if err := h.DB.First(&user, "id = ?", id).Error; err != nil {
		respondErr(c, model.NewErr(404, "user not found"))
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Where("user_id = ?", id).Delete(&model.UserRole{}).Error; e != nil {
			return e
		}
		if e := tx.Where("user_id = ?", id).Delete(&model.UserPermission{}).Error; e != nil {
			return e
		}
		if e := tx.Where("user_id = ?", id).Delete(&model.RefreshToken{}).Error; e != nil {
			return e
		}
		return tx.Delete(&user).Error
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	h.Audit.Log(middleware.AdminBy(c), "user_delete", map[string]string{"user": id}, "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ListUserRoles GET /users/{id}/roles
func (h *Handler) ListUserRoles(c *gin.Context) {
	roles, err := h.RBAC.ListUserRoles(c.Param("id"))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": roles})
}

type grantRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

// GrantRole POST /users/{id}/roles
func (h *Handler) GrantRole(c *gin.Context) {
	var req grantRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role required"})
		return
	}
	if err := h.RBAC.GrantRole(c.Param("id"), req.Role, middleware.AdminBy(c)); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// RevokeRole DELETE /users/{id}/roles/:role
func (h *Handler) RevokeRole(c *gin.Context) {
	if err := h.RBAC.RevokeRole(c.Param("id"), c.Param("role"), middleware.AdminBy(c)); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type grantPermissionRequest struct {
	Permission string     `json:"permission" binding:"required"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

// GrantPermission POST /users/{id}/permissions（直授，可带过期）
func (h *Handler) GrantPermission(c *gin.Context) {
	var req grantPermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "permission required"})
		return
	}
	if err := h.RBAC.GrantPermission(c.Param("id"), req.Permission, req.ExpiresAt, middleware.AdminBy(c)); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// RevokePermission DELETE /users/{id}/permissions/:permission
func (h *Handler) RevokePermission(c *gin.Context) {
	if err := h.RBAC.RevokePermission(c.Param("id"), c.Param("permission"), middleware.AdminBy(c)); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// UserPermissions GET /users/{id}/permissions（展开全部有效权限点；Bearer 透传或 X-Admin-Token）
func (h *Handler) UserPermissions(c *gin.Context) {
	perms, err := h.RBAC.ExpandPermissions(c.Param("id"))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"permissions": perms})
}

// userDetail 查用户 + 实时角色。
func (h *Handler) userDetail(id string) (*model.User, []string, error) {
	var user model.User
	if err := h.DB.First(&user, "id = ?", id).Error; err != nil {
		return nil, nil, model.NewErr(404, "user not found")
	}
	roles, err := h.RBAC.ListUserRoles(id)
	if err != nil {
		return nil, nil, err
	}
	return &user, roles, nil
}

func statusStr(status int8) string {
	if status == 1 {
		return "active"
	}
	return "disabled"
}
