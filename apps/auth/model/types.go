// Package model 定义 auth 服务的 GORM 数据模型（表前缀 auth_）与 DTO。
package model

import "time"

// User 用户主数据：全系统统一身份（user_id = ULID）。
type User struct {
	ID         string    `gorm:"primaryKey;size:26" json:"id"`
	Idp        string    `gorm:"size:32;not null" json:"idp"` // email / google / github / x
	IdpSubject string    `gorm:"size:255;not null" json:"idp_subject"`
	Email      string    `gorm:"size:255;index" json:"email"`
	Name       string    `gorm:"size:128" json:"name"`
	Avatar     string    `gorm:"size:512" json:"avatar"`
	WalletAddr string    `gorm:"size:64" json:"wallet_addr"`
	Status     int8      `gorm:"not null;default:1" json:"status"` // 1=active 0=disabled
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (User) TableName() string { return "auth_users" }

// Role 角色（id 即角色名，如 admin / board_approver）。
type Role struct {
	ID          string    `gorm:"primaryKey;size:32" json:"id"`
	Name        string    `gorm:"size:64;not null" json:"name"`
	Description string    `gorm:"size:255" json:"description"`
	IsBuiltin   int8      `gorm:"not null;default:0" json:"is_builtin"`
	CreatedAt   time.Time `json:"created_at"`
}

func (Role) TableName() string { return "auth_roles" }

// Permission 权限点：app:resource:action。
type Permission struct {
	ID          string    `gorm:"primaryKey;size:128" json:"id"`
	App         string    `gorm:"size:32;not null;index" json:"app"`
	Description string    `gorm:"size:255" json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

func (Permission) TableName() string { return "auth_permissions" }

// RolePermission 角色-权限点关联。
type RolePermission struct {
	RoleID       string `gorm:"primaryKey;size:32" json:"role_id"`
	PermissionID string `gorm:"primaryKey;size:128" json:"permission_id"`
}

func (RolePermission) TableName() string { return "auth_role_permissions" }

// UserRole 用户-角色分配。
type UserRole struct {
	UserID    string    `gorm:"primaryKey;size:26" json:"user_id"`
	RoleID    string    `gorm:"primaryKey;size:32" json:"role_id"`
	GrantedBy string    `gorm:"size:26" json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
}

func (UserRole) TableName() string { return "auth_user_roles" }

// UserPermission 用户直授权限点（例外/临时，可带过期）。
type UserPermission struct {
	UserID       string     `gorm:"primaryKey;size:26" json:"user_id"`
	PermissionID string     `gorm:"primaryKey;size:128" json:"permission_id"`
	ExpiresAt    *time.Time `json:"expires_at"`
	GrantedBy    string     `gorm:"size:26" json:"granted_by"`
	GrantedAt    time.Time  `json:"granted_at"`
}

func (UserPermission) TableName() string { return "auth_user_permissions" }

// OtpCode 邮箱 OTP 验证码（短命，失败计数与锁定；code 只存 SHA256）。
type OtpCode struct {
	ID        uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	Email     string     `gorm:"size:255;index;not null" json:"email"`
	CodeHash  string     `gorm:"size:64;not null" json:"-"`
	Attempts  int8       `gorm:"not null;default:0" json:"-"`
	LockedAt  *time.Time `json:"-"`
	ExpiresAt time.Time  `json:"-"`
	UsedAt    *time.Time `json:"-"`
	CreatedAt time.Time  `json:"-"`
}

func (OtpCode) TableName() string { return "auth_otp_codes" }

// RefreshToken 不透明 refresh token（支持吊销/多会话/轮换）。
type RefreshToken struct {
	ID        string     `gorm:"primaryKey;size:64" json:"id"`
	UserID    string     `gorm:"size:26;index;not null" json:"user_id"`
	Device    string     `gorm:"size:128" json:"device"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	CreatedAt time.Time  `json:"created_at"`
}

func (RefreshToken) TableName() string { return "auth_refresh_tokens" }

// TokenBlacklist access token 黑名单（jti；15min 内自然失效，定期清理）。
type TokenBlacklist struct {
	JTI       string    `gorm:"primaryKey;size:64" json:"jti"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (TokenBlacklist) TableName() string { return "auth_token_blacklist" }

// AuditLog 审计日志（只增不改）。
type AuditLog struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID    string    `gorm:"size:26;index" json:"user_id"`
	Action    string    `gorm:"size:64;not null;index" json:"action"`
	Detail    string    `gorm:"type:json" json:"detail"`
	IP        string    `gorm:"size:64" json:"ip"`
	CreatedAt time.Time `json:"created_at"`
}

func (AuditLog) TableName() string { return "auth_audit_logs" }

// ErrorLog 统一错误日志（与主项目 board 错误日志页共表，system=auth）。
type ErrorLog struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	System     string     `gorm:"size:64;index;not null" json:"system"`
	Level      string     `gorm:"size:16;index;default:error" json:"level"`
	Source     string     `gorm:"size:128;index" json:"source"`
	WorkflowID string     `gorm:"size:255;index" json:"workflow_id"`
	RunID      string     `gorm:"size:255" json:"run_id"`
	TraceID    string     `gorm:"size:64;index;default:''" json:"trace_id"`
	Message    string     `gorm:"type:text;not null" json:"message"`
	Stack      string     `gorm:"type:longtext" json:"stack"`
	Context    string     `gorm:"type:longtext" json:"context"`
	Resolved   bool       `gorm:"index;default:false" json:"resolved"`
	CreatedAt  time.Time  `gorm:"index" json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

func (ErrorLog) TableName() string { return "error_logs" }
