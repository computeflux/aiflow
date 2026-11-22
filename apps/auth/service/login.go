package service

import (
	"strings"

	"github.com/computeflux/auth/model"
)

// Login 登录统一入口：验证凭证 → upsert 用户 → 签发 token 对。
type Login struct {
	otp     *Otp
	oauth   *OAuth
	users   *UserSvc
	session *Session
}

func NewLogin(otp *Otp, oauth *OAuth, users *UserSvc, session *Session) *Login {
	return &Login{otp: otp, oauth: oauth, users: users, session: session}
}

// VerifyEmail 邮箱 OTP 登录。
func (l *Login) VerifyEmail(email, code, device, ip string) (*model.LoginResult, error) {
	if err := l.otp.Verify(email, code, ip); err != nil {
		return nil, err
	}
	user, err := l.users.Upsert(&Profile{Provider: "email", Subject: normalizeEmail(email), Email: normalizeEmail(email)})
	if err != nil {
		return nil, err
	}
	if user.Status != 1 {
		return nil, model.NewErr(403, "user disabled")
	}
	return l.session.Issue(user, device, ip)
}

// Social 社交登录：code(+PKCE) 换 IdP token → 拉资料 → upsert → 签发。
func (l *Login) Social(provider, code, codeVerifier, redirectURI, device, ip string) (*model.LoginResult, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	p, err := l.oauth.Exchange(provider, code, codeVerifier, redirectURI)
	if err != nil {
		return nil, model.NewErr(401, "oauth verification failed: "+err.Error())
	}
	user, err := l.users.Upsert(p)
	if err != nil {
		return nil, err
	}
	if user.Status != 1 {
		return nil, model.NewErr(403, "user disabled")
	}
	return l.session.Issue(user, device, ip)
}
