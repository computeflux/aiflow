// Package mailer 是 SMTP 邮件发送客户端（TLS/认证/退避重试）。
package mailer

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/config"
)

const (
	dialTimeout    = 10 * time.Second
	commandTimeout = 30 * time.Second
)

// Mailer 是 SMTP 发信客户端（stdlib net/smtp 实现，支持 SSL 465 与 STARTTLS）。
type Mailer struct {
	host     string
	port     int
	user     string
	password string
	from     string
	fromName string
}

// New 按配置构造 Mailer（发件人显示名固定 ComputeFlux）。
func New(cfg *config.Config) *Mailer {
	return &Mailer{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		user:     cfg.SMTPUser,
		password: cfg.SMTPPassword,
		from:     cfg.SMTPFrom,
		fromName: "ComputeFlux",
	}
}

// NewCustom 用显式 SMTP 参数构造 Mailer（port 0 = 465；fromName 空 = ComputeFlux）。
func NewCustom(host string, port int, user, password, fromAddress, fromName string) *Mailer {
	if fromName == "" {
		fromName = "ComputeFlux"
	}
	if port == 0 {
		port = 465
	}
	return &Mailer{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		from:     fromAddress,
		fromName: fromName,
	}
}

func (m *Mailer) Send(to []string, cc []string, subject, body, contentType string) error {
	if m.host == "" || m.user == "" {
		return fmt.Errorf("SMTP not configured")
	}
	if len(to) == 0 {
		return fmt.Errorf("no recipients")
	}
	if contentType == "" || contentType == "text" {
		contentType = "text/plain"
	} else if contentType == "html" {
		contentType = "text/html"
	}

	// 防止通过 subject 注入 SMTP 头（custom_subject 来自用户输入）
	subject = strings.ReplaceAll(strings.ReplaceAll(subject, "\r", ""), "\n", "")

	ccHeader := ""
	if len(cc) > 0 {
		ccHeader = "Cc: " + strings.Join(cc, ", ") + "\r\n"
	}

	fromName := m.fromName
	if fromName == "" {
		fromName = "ComputeFlux"
	}

	msg := fmt.Sprintf(
		"From: %s <%s>\r\n"+
			"To: %s\r\n"+
			"%s"+
			"Subject: %s\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: %s; charset=\"UTF-8\"\r\n"+
			"\r\n"+
			"%s",
		fromName, m.from, strings.Join(to, ", "), ccHeader, subject, contentType, body,
	)

	addr := fmt.Sprintf("%s:%d", m.host, m.port)

	// 465 = SMTPS (TLS direct), 587 = STARTTLS
	if m.port == 465 {
		return m.sendViaTLS(addr, to, msg)
	}
	return m.sendViaSTARTTLS(addr, to, msg)
}

// dial 建立 TCP 连接并给整个 SMTP 会话（握手+认证+发送）设置总超时，避免卡死。
func (m *Mailer) dial(addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(commandTimeout))
	return conn, nil
}

func (m *Mailer) sendViaTLS(addr string, to []string, msg string) error {
	conn, err := m.dial(addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	tlsConn := tls.Client(conn, &tls.Config{ServerName: m.host})
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("tls handshake: %w", err)
	}
	defer tlsConn.Close()

	client, err := smtp.NewClient(tlsConn, m.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	auth := smtp.PlainAuth("", m.user, m.password, m.host)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	if err := sendData(client, m.from, to, msg); err != nil {
		return err
	}
	return client.Quit()
}

func (m *Mailer) sendViaSTARTTLS(addr string, to []string, msg string) error {
	conn, err := m.dial(addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if err := client.StartTLS(&tls.Config{ServerName: m.host}); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}
	auth := smtp.PlainAuth("", m.user, m.password, m.host)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	if err := sendData(client, m.from, to, msg); err != nil {
		return err
	}
	return client.Quit()
}

// sendData 为每个收件人执行 Rcpt，同一封邮件发给所有收件人。
func sendData(client *smtp.Client, from string, to []string, msg string) error {
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	for _, r := range to {
		if err := client.Rcpt(r); err != nil {
			return fmt.Errorf("rcpt %s: %w", r, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		w.Close()
		return fmt.Errorf("write: %w", err)
	}
	return w.Close()
}
