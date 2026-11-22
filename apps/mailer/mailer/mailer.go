package mailer

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/computeflux/mailer/config"
)

const (
	dialTimeout    = 10 * time.Second
	commandTimeout = 30 * time.Second
)

type Mailer struct {
	host     string
	port     int
	user     string
	password string
	from     string
	fromName string
}

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

	if m.port == 465 {
		return m.sendViaTLS(addr, to, msg)
	}
	return m.sendViaSTARTTLS(addr, to, msg)
}

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
