package service

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/computeflux/auth/config"
	"github.com/computeflux/auth/model"
	"gorm.io/gorm"
)

// Session access/refresh 签发、续期（轮换 + 重放检测）、吊销、黑名单。
type Session struct {
	db     *gorm.DB
	cfg    *config.Config
	signer *model.Signer
	audit  *Audit
}

func NewSession(db *gorm.DB, cfg *config.Config, signer *model.Signer, audit *Audit) *Session {
	return &Session{db: db, cfg: cfg, signer: signer, audit: audit}
}

// Issue 为已登录用户签发 token 对（access JWT + 不透明 refresh）。
func (s *Session) Issue(user *model.User, device, ip string) (*model.LoginResult, error) {
	roles := s.userSvcRoles(user.ID)
	access, jti, _, err := s.signer.Issue(user.ID, roles, user.Email, user.WalletAddr)
	if err != nil {
		return nil, err
	}
	refreshID := randomHex(32)
	rt := model.RefreshToken{
		ID:        refreshID,
		UserID:    user.ID,
		Device:    device,
		ExpiresAt: time.Now().Add(time.Duration(s.cfg.AuthRefreshTTLSec) * time.Second),
		CreatedAt: time.Now(),
	}
	if err := s.db.Create(&rt).Error; err != nil {
		return nil, err
	}
	s.audit.Log(user.ID, "login", map[string]string{
		"idp": user.Idp, "device": device, "jti": jti,
	}, ip)
	return &model.LoginResult{
		AccessToken:      access,
		TokenType:        "Bearer",
		ExpiresIn:        int64(s.cfg.AuthAccessTTLSec),
		RefreshToken:     refreshID,
		RefreshExpiresIn: int64(s.cfg.AuthRefreshTTLSec),
		User: &model.UserDTO{
			ID: user.ID, Email: user.Email, Name: user.Name, Roles: roles,
		},
	}, nil
}

// Refresh 轮换 refresh：旧 token 立即作废，发新对。
// 检测到已作废 refresh 被重用 → 吊销该用户全部会话（重放攻击检测）。
func (s *Session) Refresh(refreshToken, device, ip string) (*model.LoginResult, error) {
	var rt model.RefreshToken
	err := s.db.Where("id = ?", refreshToken).First(&rt).Error
	if err == gorm.ErrRecordNotFound {
		return nil, model.NewErr(401, "invalid refresh token")
	}
	if err != nil {
		return nil, err
	}
	if rt.RevokedAt != nil {
		// 重放：吊销该用户全部会话
		now := time.Now()
		_ = s.db.Model(&model.RefreshToken{}).
			Where("user_id = ? AND revoked_at IS NULL", rt.UserID).
			Update("revoked_at", now).Error
		s.audit.Log(rt.UserID, "refresh_reuse_detected", map[string]string{"refresh": refreshToken}, ip)
		return nil, model.NewErr(401, "refresh token reuse detected, all sessions revoked")
	}
	if rt.ExpiresAt.Before(time.Now()) {
		return nil, model.NewErr(401, "refresh token expired")
	}
	now := time.Now()
	if uerr := s.db.Model(&rt).Update("revoked_at", now).Error; uerr != nil {
		return nil, uerr
	}
	var user model.User
	if uerr := s.db.First(&user, "id = ?", rt.UserID).Error; uerr != nil {
		return nil, model.NewErr(401, "user not found")
	}
	if user.Status != 1 {
		return nil, model.NewErr(403, "user disabled")
	}
	return s.Issue(&user, device, ip)
}

// Logout 登出：拉黑当前 access（jti）+ 吊销 refresh（指定则只吊销该会话，否则全部）。
func (s *Session) Logout(claims *model.Claims, refreshToken string) error {
	if claims != nil && claims.ID != "" {
		var exp time.Time
		if claims.ExpiresAt != nil {
			exp = claims.ExpiresAt.Time
		} else {
			exp = time.Now().Add(time.Duration(s.cfg.AuthAccessTTLSec) * time.Second)
		}
		_ = s.db.Where("jti = ?", claims.ID).FirstOrCreate(&model.TokenBlacklist{
			JTI: claims.ID, ExpiresAt: exp,
		}).Error
	}
	if refreshToken != "" {
		_ = s.db.Model(&model.RefreshToken{}).
			Where("id = ? AND revoked_at IS NULL", refreshToken).
			Update("revoked_at", time.Now()).Error
	} else if claims != nil {
		_ = s.db.Model(&model.RefreshToken{}).
			Where("user_id = ? AND revoked_at IS NULL", claims.Subject).
			Update("revoked_at", time.Now()).Error
	}
	if claims != nil {
		s.audit.Log(claims.Subject, "logout", map[string]string{"jti": claims.ID}, "")
	}
	return nil
}

// IsBlacklisted 查 jti 是否被拉黑（未过期）。
func (s *Session) IsBlacklisted(jti string) bool {
	var n int64
	s.db.Model(&model.TokenBlacklist{}).
		Where("jti = ? AND expires_at > ?", jti, time.Now()).
		Count(&n)
	return n > 0
}

// RevokeAll 吊销用户全部 refresh（禁用/删除时用）。
func (s *Session) RevokeAll(userID string) {
	_ = s.db.Model(&model.RefreshToken{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", time.Now()).Error
}

// Cleanup 定期清理：过期黑名单 / 过期 refresh / 已用或过期 OTP。
func (s *Session) Cleanup() {
	now := time.Now()
	_ = s.db.Where("expires_at < ?", now).Delete(&model.TokenBlacklist{}).Error
	_ = s.db.Where("expires_at < ?", now).Delete(&model.RefreshToken{}).Error
	_ = s.db.Where("used_at IS NOT NULL OR expires_at < ?", now).Delete(&model.OtpCode{}).Error
}

func (s *Session) userSvcRoles(userID string) []string {
	var rows []model.UserRole
	if err := s.db.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RoleID)
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
