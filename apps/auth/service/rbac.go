package service

import (
	"strings"
	"time"

	"github.com/computeflux/auth/model"
	"gorm.io/gorm"
)

// RBAC 角色/权限点/分配 + 权限判定（实时查库，不走 JWT 快照）。
type RBAC struct {
	db    *gorm.DB
	audit *Audit
}

func NewRBAC(db *gorm.DB, audit *Audit) *RBAC { return &RBAC{db: db, audit: audit} }

// ---- 角色 ----

// ListRoles 角色列表（含各角色权限点）。
func (r *RBAC) ListRoles() ([]model.RoleDTO, error) {
	var roles []model.Role
	if err := r.db.Order("id").Find(&roles).Error; err != nil {
		return nil, err
	}
	var rps []model.RolePermission
	_ = r.db.Find(&rps)
	permByRole := map[string][]string{}
	for _, rp := range rps {
		permByRole[rp.RoleID] = append(permByRole[rp.RoleID], rp.PermissionID)
	}
	out := make([]model.RoleDTO, 0, len(roles))
	for _, role := range roles {
		out = append(out, model.RoleDTO{
			ID: role.ID, Name: role.Name, Description: role.Description,
			IsBuiltin:   role.IsBuiltin == 1,
			Permissions: permByRole[role.ID],
		})
	}
	return out, nil
}

// CreateRole 建角色（可带初始权限点）。
func (r *RBAC) CreateRole(id, name, description string, permissions []string, adminBy string) error {
	if id == "" || name == "" {
		return model.NewErr(400, "id and name required")
	}
	role := model.Role{ID: id, Name: name, Description: description}
	if err := r.db.Create(&role).Error; err != nil {
		if strings.Contains(err.Error(), "Duplicate") {
			return model.NewErr(409, "role already exists")
		}
		return err
	}
	return r.setRolePermissions(id, permissions, adminBy, false)
}

// UpdateRole 改角色权限点（内置角色只可加不可删基础权限）。
func (r *RBAC) UpdateRole(id string, permissions []string, adminBy string) error {
	var role model.Role
	if err := r.db.First(&role, "id = ?", id).Error; err != nil {
		return model.NewErr(404, "role not found")
	}
	return r.setRolePermissions(id, permissions, adminBy, role.IsBuiltin == 1)
}

// setRolePermissions 全量替换角色权限点；builtin 时保留既有权限（只可加不可删）。
func (r *RBAC) setRolePermissions(roleID string, permissions []string, adminBy string, builtin bool) error {
	if builtin {
		var existing []model.RolePermission
		_ = r.db.Where("role_id = ?", roleID).Find(&existing)
		kept := map[string]bool{}
		for _, e := range existing {
			kept[e.PermissionID] = true
		}
		for _, p := range permissions {
			kept[p] = true
		}
		permissions = make([]string, 0, len(kept))
		for p := range kept {
			permissions = append(permissions, p)
		}
	}
	if err := r.db.Where("role_id = ?", roleID).Delete(&model.RolePermission{}).Error; err != nil {
		return err
	}
	rows := make([]model.RolePermission, 0, len(permissions))
	for _, p := range permissions {
		rows = append(rows, model.RolePermission{RoleID: roleID, PermissionID: p})
	}
	if len(rows) > 0 {
		if err := r.db.Create(&rows).Error; err != nil {
			return err
		}
	}
	r.audit.Log(adminBy, "role_update", map[string]any{"role": roleID, "permissions": permissions}, "")
	return nil
}

// DeleteRole 删角色（内置不可删；有用户绑定时 409）。
func (r *RBAC) DeleteRole(id, adminBy string) error {
	var role model.Role
	if err := r.db.First(&role, "id = ?", id).Error; err != nil {
		return model.NewErr(404, "role not found")
	}
	if role.IsBuiltin == 1 {
		return model.NewErr(403, "builtin role cannot be deleted")
	}
	var n int64
	_ = r.db.Model(&model.UserRole{}).Where("role_id = ?", id).Count(&n)
	if n > 0 {
		return model.NewErr(409, "role is assigned to users")
	}
	if err := r.db.Delete(&role).Error; err != nil {
		return err
	}
	_ = r.db.Where("role_id = ?", id).Delete(&model.RolePermission{}).Error
	r.audit.Log(adminBy, "role_delete", map[string]string{"role": id}, "")
	return nil
}

// ---- 权限点 ----

// ListPermissions 权限点列表（?app= 过滤）。
func (r *RBAC) ListPermissions(app string) ([]model.Permission, error) {
	q := r.db.Order("id")
	if app != "" {
		q = q.Where("app = ?", app)
	}
	var out []model.Permission
	return out, q.Find(&out).Error
}

// RegisterPermission 注册权限点（幂等 upsert；id 须符合 app:resource:action）。
func (r *RBAC) RegisterPermission(id, app, description string) error {
	if app == "" {
		app = firstSegment(id)
	}
	if app == "" {
		return model.NewErr(400, "permission id must be app:resource:action")
	}
	p := model.Permission{ID: id, App: app, Description: description}
	return r.db.Where("id = ?", id).FirstOrCreate(&p).Error
}

// ---- 用户-角色/直授 ----

// ListUserRoles 查用户角色。
func (r *RBAC) ListUserRoles(userID string) ([]string, error) {
	var rows []model.UserRole
	if err := r.db.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, x := range rows {
		out = append(out, x.RoleID)
	}
	return out, nil
}

// GrantRole 加角色（幂等）。
func (r *RBAC) GrantRole(userID, roleID, adminBy string) error {
	var role model.Role
	if err := r.db.First(&role, "id = ?", roleID).Error; err != nil {
		return model.NewErr(404, "role not found")
	}
	row := model.UserRole{UserID: userID, RoleID: roleID, GrantedBy: adminBy, GrantedAt: time.Now()}
	if err := r.db.Where("user_id = ? AND role_id = ?", userID, roleID).FirstOrCreate(&row).Error; err != nil {
		return err
	}
	r.audit.Log(adminBy, "role_grant", map[string]string{"user": userID, "role": roleID}, "")
	return nil
}

// RevokeRole 移除角色。
func (r *RBAC) RevokeRole(userID, roleID, adminBy string) error {
	res := r.db.Where("user_id = ? AND role_id = ?", userID, roleID).Delete(&model.UserRole{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.NewErr(404, "user does not have this role")
	}
	r.audit.Log(adminBy, "role_revoke", map[string]string{"user": userID, "role": roleID}, "")
	return nil
}

// GrantPermission 直授权限点（例外/临时，可带过期）。
func (r *RBAC) GrantPermission(userID, permissionID string, expiresAt *time.Time, adminBy string) error {
	row := model.UserPermission{
		UserID: userID, PermissionID: permissionID,
		ExpiresAt: expiresAt, GrantedBy: adminBy, GrantedAt: time.Now(),
	}
	if err := r.db.Where("user_id = ? AND permission_id = ?", userID, permissionID).FirstOrCreate(&row).Error; err != nil {
		return err
	}
	r.audit.Log(adminBy, "permission_grant", map[string]string{"user": userID, "permission": permissionID}, "")
	return nil
}

// RevokePermission 撤销直授。
func (r *RBAC) RevokePermission(userID, permissionID, adminBy string) error {
	res := r.db.Where("user_id = ? AND permission_id = ?", userID, permissionID).Delete(&model.UserPermission{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.NewErr(404, "user does not have this direct permission")
	}
	r.audit.Log(adminBy, "permission_revoke", map[string]string{"user": userID, "permission": permissionID}, "")
	return nil
}

// ---- 权限判定（实时） ----

// HasPermission 判定用户是否拥有权限点（角色 ∪ 直授，过滤过期；用户禁用 → 403）。
// 返回 (allowed, reason)；reason 非空表示拒绝原因。
func (r *RBAC) HasPermission(userID, permissionID string) (bool, string) {
	var user model.User
	if err := r.db.First(&user, "id = ?", userID).Error; err != nil {
		return false, "user_not_found"
	}
	if user.Status != 1 {
		return false, "user_disabled"
	}
	perms, err := r.ExpandPermissions(userID)
	if err != nil {
		return false, "internal_error"
	}
	for _, p := range perms {
		if p == permissionID {
			return true, ""
		}
	}
	return false, "permission_denied"
}

// ExpandPermissions 展开用户全部有效权限点（角色 ∪ 直授，过滤过期）。
func (r *RBAC) ExpandPermissions(userID string) ([]string, error) {
	var roles []model.UserRole
	if err := r.db.Where("user_id = ?", userID).Find(&roles).Error; err != nil {
		return nil, err
	}
	roleIDs := make([]string, 0, len(roles))
	for _, r := range roles {
		roleIDs = append(roleIDs, r.RoleID)
	}
	seen := map[string]bool{}
	var out []string
	if len(roleIDs) > 0 {
		var rps []model.RolePermission
		if err := r.db.Where("role_id IN ?", roleIDs).Find(&rps).Error; err != nil {
			return nil, err
		}
		for _, rp := range rps {
			if !seen[rp.PermissionID] {
				seen[rp.PermissionID] = true
				out = append(out, rp.PermissionID)
			}
		}
	}
	var ups []model.UserPermission
	if err := r.db.Where("user_id = ?", userID).Find(&ups).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	for _, up := range ups {
		if up.ExpiresAt != nil && up.ExpiresAt.Before(now) {
			continue
		}
		if !seen[up.PermissionID] {
			seen[up.PermissionID] = true
			out = append(out, up.PermissionID)
		}
	}
	return out, nil
}

func firstSegment(id string) string {
	if i := strings.Index(id, ":"); i > 0 {
		return id[:i]
	}
	return ""
}
