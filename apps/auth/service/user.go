package service

import (
	"strings"
	"time"

	"github.com/computeflux/auth/config"
	"github.com/computeflux/auth/model"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// UserSvc 用户 upsert（首次登录自动建）+ 资料合并 + 默认角色。
type UserSvc struct {
	db    *gorm.DB
	cfg   *config.Config
	audit *Audit
}

func NewUserSvc(db *gorm.DB, cfg *config.Config, audit *Audit) *UserSvc {
	return &UserSvc{db: db, cfg: cfg, audit: audit}
}

// Upsert 按 (idp, idp_subject) 查找用户；未命中且 email 已知时按 email 合并到既有用户；
// 否则新建（ULID）。首次登录自动授 `user` 基础角色；AUTH_ADMIN_EMAIL 首次登录自动授 admin。
func (u *UserSvc) Upsert(p *Profile) (*model.User, error) {
	if p.Subject == "" {
		return nil, model.NewErr(401, "idp returned no user id")
	}
	var user model.User
	err := u.db.Where("idp = ? AND idp_subject = ?", p.Provider, p.Subject).First(&user).Error
	if err == nil {
		return u.mergeProfile(&user, p), nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	// email 已知 → 合并到同一用户（同一邮箱全系统一个身份）
	if p.Email != "" {
		var byEmail model.User
		if eerr := u.db.Where("email = ?", p.Email).First(&byEmail).Error; eerr == nil {
			byEmail.Idp = p.Provider
			byEmail.IdpSubject = p.Subject
			if uerr := u.db.Save(&byEmail).Error; uerr != nil {
				return nil, uerr
			}
			return u.mergeProfile(&byEmail, p), nil
		} else if eerr != gorm.ErrRecordNotFound {
			return nil, eerr
		}
	}
	// 新建
	user = model.User{
		ID:         newULID(),
		Idp:        p.Provider,
		IdpSubject: p.Subject,
		Email:      p.Email,
		Name:       p.Name,
		Avatar:     p.Avatar,
		WalletAddr: p.WalletAddr,
		Status:     1,
	}
	if cerr := u.db.Create(&user).Error; cerr != nil {
		return nil, cerr
	}
	// 默认角色：user 基础角色（幂等）
	if gerr := u.db.Where("id = ?", "user").FirstOrCreate(&model.Role{
		ID: "user", Name: "基础用户", IsBuiltin: 1,
	}).Error; gerr != nil {
		return nil, gerr
	}
	if gerr := u.db.Where("user_id = ? AND role_id = ?", user.ID, "user").
		FirstOrCreate(&model.UserRole{UserID: user.ID, RoleID: "user", GrantedAt: time.Now()}).Error; gerr != nil {
		return nil, gerr
	}
	// admin 引导：AUTH_ADMIN_WALLET 首次登录自动授 admin（钱包身份）
	if u.cfg.AuthAdminWallet != "" && strings.EqualFold(p.WalletAddr, u.cfg.AuthAdminWallet) {
		if gerr := u.db.Where("id = ?", "admin").FirstOrCreate(&model.Role{
			ID: "admin", Name: "管理员", IsBuiltin: 1,
		}).Error; gerr != nil {
			return nil, gerr
		}
		if gerr := u.db.Where("user_id = ? AND role_id = ?", user.ID, "admin").
			FirstOrCreate(&model.UserRole{UserID: user.ID, RoleID: "admin", GrantedAt: time.Now()}).Error; gerr != nil {
			return nil, gerr
		}
		u.audit.Log(user.ID, "admin_bootstrapped", map[string]string{"wallet": p.WalletAddr}, "")
	}
	// admin 引导：AUTH_ADMIN_EMAIL 首次登录自动授 admin
	if u.cfg.AuthAdminEmail != "" && strings.EqualFold(p.Email, u.cfg.AuthAdminEmail) {
		if gerr := u.db.Where("id = ?", "admin").FirstOrCreate(&model.Role{
			ID: "admin", Name: "管理员", IsBuiltin: 1,
		}).Error; gerr != nil {
			return nil, gerr
		}
		if gerr := u.db.Where("user_id = ? AND role_id = ?", user.ID, "admin").
			FirstOrCreate(&model.UserRole{UserID: user.ID, RoleID: "admin", GrantedAt: time.Now()}).Error; gerr != nil {
			return nil, gerr
		}
		u.audit.Log(user.ID, "admin_bootstrapped", map[string]string{"email": p.Email}, "")
	}
	return &user, nil
}

// mergeProfile 更新资料字段（只覆盖非空值）。
func (u *UserSvc) mergeProfile(user *model.User, p *Profile) *model.User {
	changed := false
	if p.Email != "" && p.Email != user.Email {
		user.Email = p.Email
		changed = true
	}
	if p.Name != "" && p.Name != user.Name {
		user.Name = p.Name
		changed = true
	}
	if p.Avatar != "" && p.Avatar != user.Avatar {
		user.Avatar = p.Avatar
		changed = true
	}
	if changed {
		_ = u.db.Save(user)
	}
	return user
}

// Roles 实时查用户角色（/me 与签发 token 用）。
func (u *UserSvc) Roles(userID string) []string {
	var rows []model.UserRole
	if err := u.db.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RoleID)
	}
	return out
}

func newULID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), ulid.DefaultEntropy()).String()
}
