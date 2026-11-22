package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/computeflux/auth/model"
	"gorm.io/gorm"
)

const (
	otpTTL         = 5 * time.Minute
	otpSendGap     = 60 * time.Second
	otpMaxAttempts = 5
	otpLockTTL     = 30 * time.Minute
)

// Otp 邮箱 OTP 生成/验证/失败锁定（5 次失败锁 30min；code 只存 SHA256）。
type Otp struct {
	db     *gorm.DB
	mailer *Mailer
	audit  *Audit
}

func NewOtp(db *gorm.DB, mailer *Mailer, audit *Audit) *Otp {
	return &Otp{db: db, mailer: mailer, audit: audit}
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// Request 发送验证码：同邮箱 60s 一次（429）；已锁定（423）。
func (o *Otp) Request(email, ip string) error {
	email = normalizeEmail(email)
	if email == "" || !strings.Contains(email, "@") {
		return model.NewErr(400, "invalid email")
	}
	var latest model.OtpCode
	err := o.db.Where("email = ?", email).Order("id DESC").First(&latest).Error
	if err == nil {
		if latest.LockedAt != nil && latest.LockedAt.After(time.Now()) {
			return model.NewErr(423, "too many failed attempts, locked for 30 minutes")
		}
		if time.Since(latest.CreatedAt) < otpSendGap {
			return model.NewErr(429, "request too frequent, try again in 60s")
		}
	} else if err != gorm.ErrRecordNotFound {
		return err
	}
	code := genCode6()
	row := model.OtpCode{
		Email:     email,
		CodeHash:  hashCode(code),
		ExpiresAt: time.Now().Add(otpTTL),
		CreatedAt: time.Now(),
	}
	if err := o.db.Create(&row).Error; err != nil {
		return err
	}
	if err := o.mailer.SendOTP(email, code); err != nil {
		o.audit.Log("", "otp_send_failed", map[string]string{"email": email, "error": err.Error()}, ip)
		return fmt.Errorf("send otp email: %w", err)
	}
	o.audit.Log("", "otp_sent", map[string]string{"email": email}, ip)
	return nil
}

// Verify 校验验证码（一次性）；失败计数，5 次锁定 30min。
func (o *Otp) Verify(email, code, ip string) error {
	email = normalizeEmail(email)
	var row model.OtpCode
	err := o.db.Where("email = ? AND used_at IS NULL AND expires_at > ?", email, time.Now()).
		Order("id DESC").First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.NewErr(401, "invalid or expired code")
	}
	if err != nil {
		return err
	}
	if row.LockedAt != nil && row.LockedAt.After(time.Now()) {
		return model.NewErr(423, "too many failed attempts, locked for 30 minutes")
	}
	if row.CodeHash != hashCode(strings.TrimSpace(code)) {
		row.Attempts++
		updates := map[string]any{"attempts": row.Attempts}
		if row.Attempts >= otpMaxAttempts {
			lockUntil := time.Now().Add(otpLockTTL)
			updates["locked_at"] = lockUntil
			o.audit.Log("", "otp_locked", map[string]string{"email": email}, ip)
		}
		if uerr := o.db.Model(&row).Updates(updates).Error; uerr != nil {
			return uerr
		}
		return model.NewErr(401, "invalid code")
	}
	now := time.Now()
	if uerr := o.db.Model(&row).Update("used_at", now).Error; uerr != nil {
		return uerr
	}
	return nil
}

func genCode6() string {
	b := make([]byte, 8)
	if _, rerr := rand.Read(b); rerr != nil {
		panic(rerr)
	}
	v := int(hexToUint64(b) % 900000)
	return fmt.Sprintf("%06d", 100000+v)
}

func hexToUint64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}
