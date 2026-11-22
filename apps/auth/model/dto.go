package model

import "time"

// ---- 登录/会话 DTO ----

// UserDTO 登录响应里的用户摘要。
type UserDTO struct {
	ID    string   `json:"id"`
	Email string   `json:"email,omitempty"`
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles"`
}

// LoginResult 登录/刷新统一响应（字段名对齐 web3auth SDK 的 REST 约定）。
type LoginResult struct {
	AccessToken      string   `json:"access_token"`
	TokenType        string   `json:"token_type"`
	ExpiresIn        int64    `json:"expires_in"`
	RefreshToken     string   `json:"refresh_token"`
	RefreshExpiresIn int64    `json:"refresh_expires_in"`
	User             *UserDTO `json:"user"`
}

// MeResponse GET /me 响应。
type MeResponse struct {
	ID         string   `json:"id"`
	Email      string   `json:"email,omitempty"`
	Name       string   `json:"name,omitempty"`
	Avatar     string   `json:"avatar,omitempty"`
	WalletAddr string   `json:"wallet_addr,omitempty"`
	Roles      []string `json:"roles"`
	Status     string   `json:"status"`
}

// SessionDTO 用户详情里的会话列表项。
type SessionDTO struct {
	ID        string    `json:"id"`
	Device    string    `json:"device,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DirectPermissionDTO 用户详情里的直授权限项。
type DirectPermissionDTO struct {
	Permission string     `json:"permission"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// UserDetailResponse GET /users/{id} 响应。
type UserDetailResponse struct {
	ID                string                `json:"id"`
	Email             string                `json:"email,omitempty"`
	Name              string                `json:"name,omitempty"`
	Avatar            string                `json:"avatar,omitempty"`
	WalletAddr        string                `json:"wallet_addr,omitempty"`
	Status            string                `json:"status"`
	Roles             []string              `json:"roles"`
	DirectPermissions []DirectPermissionDTO `json:"direct_permissions"`
	Sessions          []SessionDTO          `json:"sessions"`
	CreatedAt         time.Time             `json:"created_at"`
}

// RoleDTO 角色列表项（含权限点）。
type RoleDTO struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	IsBuiltin   bool     `json:"is_builtin"`
	Permissions []string `json:"permissions"`
}

// ---- 错误 ----

// AppError 带 HTTP 状态码的业务错误。
type AppError struct {
	Status int
	Msg    string
}

func (e *AppError) Error() string { return e.Msg }

func NewErr(status int, msg string) *AppError { return &AppError{Status: status, Msg: msg} }
