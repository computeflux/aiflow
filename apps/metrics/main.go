// Command sslmon 是项目指标监控工具: 监控服务健康(HTTP 探测)与 TLS 证书状态,
// 支持邮件告警。所有配置数据由 MySQL monitor_config 表维护(见 dbconfig.go)。
//
// Usage:
//
//	sslmon                        # one-shot; email alert if anything is not OK
//	sslmon -loop                  # continuous; email on status changes / recovery
//	sslmon -mail-test             # send a test email and exit
//	sslmon -hosts a.com,b.com -json  # explicit hosts (highest priority), JSON output
//	sslmon -http                  # HTTP server: UI + GET /api/status (port 8003)
//
// Exit code: 0 all OK, 1 at least one WARN, 2 at least one EXPIRED/ERROR.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

const (
	statusOK      = "OK"
	statusWarn    = "WARN"
	statusExpired = "EXPIRED"
	statusError   = "ERROR"
)

const defaultHosts = "test.computeflux.ai,test-api.computeflux.ai,computeflux.ai"

// certReport holds the monitoring result of a single host.
type certReport struct {
	Host       string   `json:"host"`
	Status     string   `json:"status"`
	DaysLeft   float64  `json:"days_left"`
	Subject    string   `json:"subject,omitempty"`
	Issuer     string   `json:"issuer,omitempty"`
	SANs       []string `json:"sans,omitempty"`
	NotBefore  string   `json:"not_before,omitempty"`
	NotAfter   string   `json:"not_after,omitempty"`
	ChainError string   `json:"chain_error,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// severity ranks reports so the process can pick a proper exit code.
func (r certReport) severity() int {
	switch r.Status {
	case statusError, statusExpired:
		return 2
	case statusWarn:
		return 1
	default:
		return 0
	}
}

// smtpConfig describes the SMTP server used for alert emails.
type smtpConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"` // 587 STARTTLS, 465 implicit TLS
	Username string `json:"username"`
	Password string `json:"password"` // may reference an env var as "$NAME"
	From     string `json:"from"`
	StartTLS *bool  `json:"starttls"`
}

// config 是完整配置布局: MySQL monitor_config 表中的单行 JSON(面板可读写, 30s 热加载)。
// 配置库连接本身由 MONITOR_DB_DSN 环境变量(或内置默认)决定, 不在配置内。
type config struct {
	Hosts       []string       `json:"hosts"`
	Port        int            `json:"port"`
	WarnDays    int            `json:"warn_days"`
	Services    []serviceEntry `json:"services"`    // 项目服务健康检查列表(可选)
	Deployables []deployEntry  `json:"deployables"` // 可部署应用列表(可选, 面板部署控制)
	Renewal     renewalConfig  `json:"renewal"`     // Let's Encrypt 证书续签(可选, email 非空时启用)
	Interval    string         `json:"interval"`
	AlertTo     []string       `json:"alert_to"`
	SMTP        smtpConfig     `json:"smtp"`
}

// hostsOverride 是 -hosts 命令行参数, 最高优先级; 仅临时覆盖, 不回写数据库。
var hostsOverride []string

// effectiveHosts 返回生效的监控主机列表,
// 优先级: -hosts 参数 > SSL_HOSTS 环境变量 > 数据库配置 > 内置默认。
func effectiveHosts(cfg *config) []string {
	if len(hostsOverride) > 0 {
		return hostsOverride
	}
	if h := os.Getenv("SSL_HOSTS"); h != "" {
		return splitHosts(h)
	}
	if cfg != nil && len(cfg.Hosts) > 0 {
		return cfg.Hosts
	}
	return splitHosts(defaultHosts)
}

// resolveServiceURL 把服务地址中的 xxxx 主机占位符替换为 MONITOR_HOST
// (容器部署时默认 host.docker.internal, 用于从容器内探测宿主机上的服务);
// 不含占位符的地址(如 127.0.0.1)原样使用。
func resolveServiceURL(raw string) string {
	if !strings.Contains(raw, "xxxx") {
		return raw
	}
	host := os.Getenv("MONITOR_HOST")
	if host == "" {
		host = "host.docker.internal"
	}
	return strings.Replace(raw, "xxxx", host, 1)
}

func splitHosts(s string) []string {
	var hosts []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func main() {
	hostsFlag := flag.String("hosts", "", "hosts to check (highest priority, overrides database config)")
	portFlag := flag.Int("port", 0, "TLS port (overrides database config)")
	warnFlag := flag.Int("warn-days", 0, "warn when a cert expires within N days (overrides database config)")
	loop := flag.Bool("loop", false, "keep monitoring in a loop")
	intervalFlag := flag.Duration("interval", 0, "interval between checks in loop mode (overrides database config)")
	jsonOut := flag.Bool("json", false, "print JSON instead of a table")
	mailTest := flag.Bool("mail-test", false, "send a test email and exit")
	httpServe := flag.Bool("http", false, "run HTTP server (UI + /api/status) instead of CLI mode")
	httpPortFlag := flag.Int("http-port", 0, "HTTP server port (default 8003, or $SSL_HTTP_PORT)")
	flag.Parse()

	if h := splitHosts(*hostsFlag); len(h) > 0 {
		hostsOverride = h
	}

	// 所有配置数据由数据库维护(见 dbconfig.go): 先连库再取配置。
	store, storeErr := openConfigStore()
	var cfg *config
	if store != nil {
		cfg = store.get()
	}

	// HTTP 服务模式: -http 或环境变量 SSL_HTTP=1; 端口默认 8003, 可用
	// -http-port 或环境变量 SSL_HTTP_PORT 覆盖。
	httpEnabled := *httpServe || os.Getenv("SSL_HTTP") == "1"
	httpPort := 8003
	if hp := os.Getenv("SSL_HTTP_PORT"); hp != "" {
		if n, err := strconv.Atoi(hp); err == nil && n > 0 {
			httpPort = n
		}
	}
	if *httpPortFlag > 0 {
		httpPort = *httpPortFlag
	}
	if httpEnabled {
		// ---- 无回退: 数据库不可达时不启动, 避免配置不一致的静默降级 ----
		if storeErr != nil {
			fmt.Fprintln(os.Stderr, "http server error:", storeErr)
			os.Exit(2)
		}
		if err := startHTTP(store, httpPort); err != nil {
			fmt.Fprintln(os.Stderr, "http server error:", err)
			os.Exit(2)
		}
		return
	}

	// CLI 模式: 数据库不可达时不阻塞检查, 仅降级为无邮件告警(用内置默认主机)
	if storeErr != nil {
		fmt.Fprintln(os.Stderr, "note: 配置数据库不可达 (", storeErr, "), using defaults, email alerts disabled")
	}

	hosts := effectiveHosts(cfg)
	if len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "no hosts to check")
		os.Exit(2)
	}

	port := 443
	if cfg != nil && cfg.Port > 0 {
		port = cfg.Port
	}
	if *portFlag > 0 {
		port = *portFlag
	}

	warnDays := 30
	if cfg != nil && cfg.WarnDays > 0 {
		warnDays = cfg.WarnDays
	}
	if *warnFlag > 0 {
		warnDays = *warnFlag
	}

	interval := time.Hour
	if cfg != nil && cfg.Interval != "" {
		if d, err := time.ParseDuration(cfg.Interval); err == nil && d > 0 {
			interval = d
		} else {
			fmt.Fprintln(os.Stderr, "note: invalid interval in config, using 1h")
		}
	}
	if *intervalFlag > 0 {
		interval = *intervalFlag
	}

	m := newMailer(cfg)

	if *mailTest {
		if m == nil {
			fmt.Fprintln(os.Stderr, "email not configured: alert_to and smtp.host are required in the config file")
			os.Exit(2)
		}
		body := fmt.Sprintf("This is a test email from sslmon, sent at %s.\nIf you received it, the SMTP settings work.\n",
			time.Now().Format("2006-01-02 15:04:05"))
		if err := m.send("[sslmon] test email", body); err != nil {
			fmt.Fprintln(os.Stderr, "test email failed:", err)
			os.Exit(2)
		}
		fmt.Println("test email sent to", strings.Join(m.to, ", "))
		return
	}

	// previous per-host status in loop mode, for change/recovery detection
	prev := map[string]string{}
	for {
		reports := checkAll(hosts, port, warnDays)
		if *jsonOut {
			printJSON(reports)
		} else {
			printTable(reports)
		}

		if m != nil {
			if *loop {
				alert, recovered := diffStatus(prev, reports)
				if alert {
					if err := m.sendAlert(reports); err != nil {
						fmt.Fprintln(os.Stderr, "alert email failed:", err)
					}
				}
				if len(recovered) > 0 {
					if err := m.sendRecovery(recovered); err != nil {
						fmt.Fprintln(os.Stderr, "recovery email failed:", err)
					}
				}
				for _, r := range reports {
					prev[r.Host] = r.Status
				}
			} else if maxSeverity(reports) >= 1 {
				if err := m.sendAlert(reports); err != nil {
					fmt.Fprintln(os.Stderr, "alert email failed:", err)
				}
			}
		}

		if !*loop {
			os.Exit(maxSeverity(reports))
		}
		time.Sleep(interval)
	}
}

// diffStatus reports whether any host changed into a non-OK state since prev,
// and which hosts recovered to OK.
func diffStatus(prev map[string]string, reports []certReport) (bool, []string) {
	alert := false
	var recovered []string
	for _, r := range reports {
		old, known := prev[r.Host]
		if !known || r.Status != old {
			if r.Status != statusOK {
				alert = true
			}
		}
		if known && old != statusOK && r.Status == statusOK {
			recovered = append(recovered, r.Host)
		}
	}
	return alert, recovered
}

// checkAll runs the per-host checks in parallel and returns reports in input order.
func checkAll(hosts []string, port, warnDays int) []certReport {
	reports := make([]certReport, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			reports[i] = checkHost(host, port, warnDays)
		}(i, host)
	}
	wg.Wait()
	return reports
}

// checkHost fetches the leaf TLS certificate of host:port and evaluates its
// validity window and chain.
func checkHost(host string, port, warnDays int) certReport {
	r := certReport{Host: host}

	addr := net.JoinHostPort(host, fmt.Sprint(port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	// InsecureSkipVerify only relaxes the handshake so we can still read the
	// certificate of a misconfigured/broken chain; the chain is verified
	// manually below and reported as ChainError.
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
	})
	if err != nil {
		r.Status = statusError
		r.Error = err.Error()
		return r
	}
	defer conn.Close()

	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		r.Status = statusError
		r.Error = "server presented no certificate"
		return r
	}
	leaf := state.PeerCertificates[0]

	// Verify the full chain against the system roots (loaded manually since
	// Go 1.26 removed x509.CertPool.AddSystemCerts).
	roots := x509.NewCertPool()
	for _, file := range []string{
		"/etc/ssl/certs/ca-certificates.crt",                // Debian/Ubuntu
		"/etc/pki/tls/certs/ca-bundle.crt",                  // RHEL/CentOS
		"/etc/ssl/cert.pem",                                 // macOS
		"/etc/pki/tls/cacert.pem",                           // Alpine/BusyBox
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // Fedora
	} {
		data, err := os.ReadFile(file)
		if err == nil {
			roots.AppendCertsFromPEM(data)
			break
		}
	}
	if len(roots.Subjects()) > 0 {
		intermediates := x509.NewCertPool()
		for _, c := range state.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		if _, verr := leaf.Verify(x509.VerifyOptions{
			DNSName:       host,
			Roots:         roots,
			Intermediates: intermediates,
		}); verr != nil {
			r.ChainError = verr.Error()
		}
	}

	daysLeft := math.Round(time.Until(leaf.NotAfter).Hours()/24*10) / 10
	r.DaysLeft = daysLeft
	r.Subject = leaf.Subject.CommonName
	r.Issuer = leaf.Issuer.CommonName
	r.SANs = leaf.DNSNames
	r.NotBefore = leaf.NotBefore.UTC().Format(time.RFC3339)
	r.NotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)

	switch {
	case daysLeft < 0:
		r.Status = statusExpired
	case daysLeft <= float64(warnDays):
		r.Status = statusWarn
	default:
		r.Status = statusOK
	}
	if r.ChainError != "" && r.Status == statusOK {
		r.Status = statusWarn
	}
	return r
}

func maxSeverity(reports []certReport) int {
	max := 0
	for _, r := range reports {
		if s := r.severity(); s > max {
			max = s
		}
	}
	return max
}

func printTable(reports []certReport) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "SSL certificate check\t%s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "host\tstatus\tdays-left\tnot-after\tissuer\n")
	for _, r := range reports {
		notAfter := "n/a"
		if r.NotAfter != "" {
			// r.NotAfter is RFC3339 UTC; reformat for the table
			if t, err := time.Parse(time.RFC3339, r.NotAfter); err == nil {
				notAfter = t.Local().Format("2006-01-02 15:04:05")
			}
		}
		days := "n/a"
		if r.Status != statusError {
			days = fmt.Sprintf("%.1f", r.DaysLeft)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Host, r.Status, days, notAfter, r.Issuer)
		if r.ChainError != "" {
			fmt.Fprintf(w, "    chain error: %s\n", r.ChainError)
		}
		if r.Error != "" {
			fmt.Fprintf(w, "    error: %s\n", r.Error)
		}
	}
	w.Flush()
}

func printJSON(reports []certReport) {
	out, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "json encode error:", err)
		os.Exit(2)
	}
	fmt.Println(string(out))
}

// ---------------------------------------------------------------------------
// email
// ---------------------------------------------------------------------------

// mailer sends alert emails via SMTP.
type mailer struct {
	smtp smtpConfig
	from string
	to   []string
}

func newMailer(cfg *config) *mailer {
	if cfg == nil || len(cfg.AlertTo) == 0 || cfg.SMTP.Host == "" {
		return nil
	}
	from := cfg.SMTP.From
	if from == "" {
		from = cfg.SMTP.Username
	}
	if from == "" {
		from = "sslmon@localhost"
	}
	return &mailer{smtp: cfg.SMTP, from: from, to: cfg.AlertTo}
}

// sendAlert emails all hosts that are currently not OK.
func (m *mailer) sendAlert(reports []certReport) error {
	var bad []certReport
	for _, r := range reports {
		if r.Status != statusOK {
			bad = append(bad, r)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	names := make([]string, 0, len(bad))
	for _, r := range bad {
		names = append(names, fmt.Sprintf("%s (%s)", r.Host, r.Status))
	}
	subject := "[sslmon] SSL certificate alert: " + strings.Join(names, ", ")

	var b strings.Builder
	fmt.Fprintf(&b, "SSL certificate monitoring alert, %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	b.WriteString(tableOf(bad))
	for _, r := range bad {
		fmt.Fprintf(&b, "- %s: %s", r.Host, r.Status)
		if r.DaysLeft < 0 {
			fmt.Fprintf(&b, ", expired %.1f days ago", -r.DaysLeft)
		} else {
			fmt.Fprintf(&b, ", expires in %.1f days", r.DaysLeft)
		}
		fmt.Fprintf(&b, "\n  subject: %s\n  issuer:  %s\n  valid:   %s ~ %s\n", r.Subject, r.Issuer, r.NotBefore, r.NotAfter)
		if len(r.SANs) > 0 {
			fmt.Fprintf(&b, "  sans:    %s\n", strings.Join(r.SANs, ", "))
		}
		if r.ChainError != "" {
			fmt.Fprintf(&b, "  chain:   %s\n", r.ChainError)
		}
		if r.Error != "" {
			fmt.Fprintf(&b, "  error:   %s\n", r.Error)
		}
	}
	return m.send(subject, b.String())
}

// sendRecovery emails which hosts are back to OK.
func (m *mailer) sendRecovery(hosts []string) error {
	subject := "[sslmon] recovered (OK): " + strings.Join(hosts, ", ")
	body := fmt.Sprintf("The following hosts now present a healthy TLS certificate, %s:\n\n  %s\n",
		time.Now().Format("2006-01-02 15:04:05"), strings.Join(hosts, "\n  "))
	return m.send(subject, body)
}

// tableOf renders the given reports as a plain-text table.
func tableOf(reports []certReport) string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "host\tstatus\tdays-left\tnot-after\tissuer\n")
	for _, r := range reports {
		notAfter := "n/a"
		if r.NotAfter != "" {
			if t, err := time.Parse(time.RFC3339, r.NotAfter); err == nil {
				notAfter = t.Local().Format("2006-01-02 15:04:05")
			}
		}
		days := "n/a"
		if r.Status != statusError {
			days = fmt.Sprintf("%.1f", r.DaysLeft)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Host, r.Status, days, notAfter, r.Issuer)
	}
	w.Flush()
	return sb.String()
}

// send delivers subject+body to all configured recipients.
func (m *mailer) send(subject, body string) error {
	host := m.smtp.Host
	port := m.smtp.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))

	var (
		client *smtp.Client
		err    error
	)
	switch {
	case port == 465: // implicit TLS
		tc, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		if client, err = smtp.NewClient(tc, host); err != nil {
			tc.Close()
			return err
		}
	default:
		if client, err = smtp.Dial(addr); err != nil {
			return err
		}
		advertisesStartTLS := false
		if m.smtp.StartTLS == nil {
			advertisesStartTLS, _ = client.Extension("STARTTLS")
		}
		switch {
		case m.smtp.StartTLS == nil && !advertisesStartTLS:
			// default: STARTTLS only when the server advertises it
		case m.smtp.StartTLS == nil || *m.smtp.StartTLS:
			if err = client.StartTLS(&tls.Config{ServerName: host}); err != nil {
				client.Close()
				return err
			}
		}
	}
	defer client.Close()

	if m.smtp.Username != "" {
		password := m.smtp.Password
		if strings.HasPrefix(password, "$") { // support "$ENV_VAR" references
			password = os.Getenv(password[1:])
		}
		if err = client.Auth(smtp.PlainAuth("", m.smtp.Username, password, host)); err != nil {
			return err
		}
	}

	if err = client.Mail(m.from); err != nil {
		return err
	}
	for _, to := range m.to {
		if err = client.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	header := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\nDate: %s\nMIME-Version: 1.0\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: 8bit\n\n",
		m.from, strings.Join(m.to, ", "), subject, time.Now().Format(time.RFC1123Z))
	if _, err = io.WriteString(w, header+body); err != nil {
		return err
	}
	return w.Close()
}
