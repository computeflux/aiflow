package service

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"github.com/computeflux/auth/config"
)

// Mailer 发 OTP 邮件（SMTP 直发，复用根 .env 的 SMTP_*）。
type Mailer struct {
	cfg *config.Config
}

func NewMailer(cfg *config.Config) *Mailer { return &Mailer{cfg: cfg} }

// Enabled 判断 SMTP 是否配置完整。
func (m *Mailer) Enabled() bool {
	return m.cfg.SMTPHost != "" && m.cfg.SMTPUser != ""
}

// SendOTP 发送 6 位验证码邮件。
func (m *Mailer) SendOTP(to, code string) error {
	if !m.Enabled() {
		return fmt.Errorf("SMTP not configured (SMTP_HOST/SMTP_USER missing)")
	}
	subject := "ComputeFlux 登录验证码"
	body := fmt.Sprintf("您的登录验证码是：%s\n\n5 分钟内有效。如非本人操作请忽略。", code)
	return m.send(to, subject, body)
}

func (m *Mailer) send(to, subject, body string) error {
	host := m.cfg.SMTPHost
	port := m.cfg.SMTPPort
	if port == "" {
		port = "465"
	}
	var client *smtp.Client
	var err error
	switch port {
	case "465": // 隐式 TLS
		conn, derr := tls.Dial("tcp", net.JoinHostPort(host, port), &tls.Config{ServerName: host})
		if derr != nil {
			return derr
		}
		client, err = smtp.NewClient(conn, host)
	default: // 25 / 587
		client, err = smtp.Dial(net.JoinHostPort(host, port))
		if err == nil && port == "587" {
			if err = client.StartTLS(&tls.Config{ServerName: host}); err != nil {
				client.Close()
				return err
			}
		}
	}
	if err != nil {
		return err
	}
	defer client.Close()

	if m.cfg.SMTPPassword != "" {
		if err = client.Auth(smtp.PlainAuth("", m.cfg.SMTPUser, m.cfg.SMTPPassword, host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	from := m.cfg.SMTPFrom
	if from == "" {
		from = m.cfg.SMTPUser
	}
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	}, "\r\n")
	if err = client.Mail(from); err != nil {
		return err
	}
	if err = client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write([]byte(msg)); err != nil {
		return err
	}
	return w.Close()
}
