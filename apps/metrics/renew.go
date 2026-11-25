package main

// 证书续签 (Let's Encrypt / ACME):
//   - 自动: 监控证书剩余天数 <= renewal.days(默认 10 天)时自动签发新证书
//   - 手动: 面板按钮 POST /api/renew
//   - 验证方式: DNS-01(域名在阿里云 DNS, 无需域名指向本机);
//     凭证 ALICLOUD_ACCESS_KEY / ALICLOUD_SECRET_KEY(需 AliyunDNSFullAccess 权限)
//   - 产物: /app/certs/<host>/{cert,fullchain,privkey}.pem + meta.json(卷 metrics_certs);
//     ACME 账号私钥: /app/certs/.acme/account.key
//   - 下载: GET /api/cert/{host}/{cert|fullchain|privkey}(私钥需 token)

import (
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"
)

const (
	renewCertDir    = "/app/certs"
	renewAcmeDir    = "/app/certs/.acme"
	renewLoopEvery  = 30 * time.Minute
	renewDNSWait    = 20 * time.Second // TXT 记录传播等待(alidns 通常 <10s 生效)
	renewDNSTimeout = 5 * time.Minute
)

// renewResolvers 是 lego 做 zone 探测(SOA 查询)与权威 NS 查询时使用的解析器。
// 必须显式指定: computeflux.ai 根域名是 CNAME(七牛 CDN), 容器系统解析链路
// (Docker DNS → 宿主 systemd-resolved)对 SOA 查询返回 CNAME 跟随结果, 会让 lego
// 一路探测到 TLD "ai."(报 "zone ai. not found"); 公共解析器返回正确的 zone SOA。
var renewResolvers = []string{"223.5.5.5:53", "223.6.6.6:53"}

type renewalConfig struct {
	Enabled bool   `json:"enabled"`
	Days    int    `json:"days"`
	Email   string `json:"email"`
	DNS     string `json:"dns"`     // DNS-01 provider, 默认 alidns
	Staging bool   `json:"staging"` // true = LE staging 环境(仅测试用)
}

// certMeta 是签发元数据(/app/certs/<host>/meta.json)。
type certMeta struct {
	Host      string    `json:"host"`
	IssuedAt  time.Time `json:"issued_at"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// renewalInfo 是单个主机的续签状态(/api/status 的 renewals 字段)。
type renewalInfo struct {
	Host         string  `json:"host"`
	Port         int     `json:"port,omitempty"` // 监控目标端口(与证书监控一致; 续签只对域名签发, 端口仅用于展示对应)
	DaysLeft     float64 `json:"days_left"`
	Due          bool    `json:"due"`        // 达到自动续签阈值
	CertReady    bool    `json:"cert_ready"` // 已有新证书可下载
	CertNotAfter string  `json:"cert_not_after,omitempty"`
	LastTry      string  `json:"last_try,omitempty"`
	LastError    string  `json:"last_error,omitempty"`
}

type renews struct {
	mu     sync.Mutex
	store  configStore // 配置现取自数据库(热加载), 每次签发/检查时实时读取
	hostRe *regexp.Regexp
	info   map[string]*renewalInfo
}

// renewalCfg 返回当前生效的续签配置(默认 days=10, dns=alidns)。
func (r *renews) renewalCfg() renewalConfig {
	rc := r.store.get().Renewal
	if rc.Days <= 0 {
		rc.Days = 10
	}
	if rc.DNS == "" {
		rc.DNS = "alidns"
	}
	return rc
}

// monitoredHosts 返回当前生效的监控主机列表(续签白名单校验用;
// host:port 条目只取主机名——ACME 只对域名签发, 与端口无关)。
func (r *renews) monitoredHosts() []string {
	var out []string
	for _, entry := range effectiveHosts(r.store.get()) {
		host, _, err := splitHostPortEntry(entry)
		if err != nil {
			host = entry
		}
		out = append(out, host)
	}
	return out
}

func newRenewer(store configStore) *renews {
	r := &renews{
		store:  store,
		hostRe: regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`),
		info:   map[string]*renewalInfo{},
	}
	// 从磁盘恢复已签发状态(meta.json)
	if entries, err := os.ReadDir(renewCertDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			var m certMeta
			b, err := os.ReadFile(filepath.Join(renewCertDir, e.Name(), "meta.json"))
			if err != nil || json.Unmarshal(b, &m) != nil {
				continue
			}
			r.info[e.Name()] = &renewalInfo{
				Host:         e.Name(),
				CertReady:    true,
				CertNotAfter: m.NotAfter.UTC().Format(time.RFC3339),
				LastTry:      m.IssuedAt.UTC().Format(time.RFC3339),
			}
		}
	}
	return r
}

// leUser 实现 lego 的 registration.User 接口; 账号私钥持久化, 跨重启复用同一 ACME 账号。
type leUser struct {
	email string
	key   crypto.PrivateKey
}

func (u *leUser) GetEmail() string                        { return u.email }
func (u *leUser) GetRegistration() *registration.Resource { return nil }
func (u *leUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// loadOrCreateAccountKey 读取 ACME 账号私钥, 不存在则生成并保存。
func loadOrCreateAccountKey() (crypto.PrivateKey, error) {
	p := filepath.Join(renewAcmeDir, "account.key")
	if b, err := os.ReadFile(p); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				return k, nil
			}
		}
	}
	key, err := certcrypto.GeneratePrivateKey(certcrypto.EC256)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(renewAcmeDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// issue 通过 ACME 为 host 签发证书并落盘, 返回新证书的到期时间(RFC3339)。
func (r *renews) issue(host string) (string, error) {
	if !r.hostRe.MatchString(host) {
		return "", fmt.Errorf("非法主机名: %s", host)
	}
	if os.Getenv("ALICLOUD_ACCESS_KEY") == "" || os.Getenv("ALICLOUD_SECRET_KEY") == "" {
		return "", fmt.Errorf("未配置阿里云 AccessKey: 请在 .env 设置 ALICLOUD_ACCESS_KEY / ALICLOUD_SECRET_KEY")
	}

	rc := r.renewalCfg()
	if rc.Email == "" {
		return "", errors.New("未设置 renewal.email: 无法签发(请在面板设置或数据库中配置)")
	}
	key, err := loadOrCreateAccountKey()
	if err != nil {
		return "", fmt.Errorf("ACME 账号密钥: %w", err)
	}
	cfg := lego.NewConfig(&leUser{email: rc.Email, key: key})
	if rc.Staging {
		cfg.CADirURL = lego.LEDirectoryStaging
	} else {
		cfg.CADirURL = lego.LEDirectoryProduction
	}
	cfg.Certificate.KeyType = certcrypto.EC256
	client, err := lego.NewClient(cfg)
	if err != nil {
		return "", err
	}
	provider, err := dns.NewDNSChallengeProviderByName(rc.DNS)
	if err != nil {
		return "", fmt.Errorf("DNS provider %s 不可用: %w", rc.DNS, err)
	}
	if err := client.Challenge.SetDNS01Provider(provider,
		dns01.AddDNSTimeout(renewDNSTimeout),
		dns01.PropagationWait(renewDNSWait, false),
		dns01.AddRecursiveNameservers(renewResolvers),
	); err != nil {
		return "", err
	}
	// 注册 ACME 账号(已存在则复用)
	if _, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true}); err != nil {
		return "", fmt.Errorf("ACME 账号注册: %w", err)
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{host}})
	if err != nil {
		return "", err
	}
	return r.saveCert(host, res)
}

// saveCert 落盘签发产物(PEM)与元数据, 更新内存状态。
func (r *renews) saveCert(host string, res *certificate.Resource) (string, error) {
	dir := filepath.Join(renewCertDir, host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	block, _ := pem.Decode(res.Certificate)
	if block == nil {
		return "", fmt.Errorf("无法解析签发的证书")
	}
	xc, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	files := map[string][]byte{
		"cert":      res.Certificate,
		"fullchain": append(append([]byte{}, res.Certificate...), res.IssuerCertificate...),
		"privkey":   res.PrivateKey,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".pem"), data, 0o600); err != nil {
			return "", err
		}
	}
	meta := certMeta{Host: host, IssuedAt: time.Now(), NotBefore: xc.NotBefore, NotAfter: xc.NotAfter}
	b, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o600); err != nil {
		return "", err
	}
	notAfter := xc.NotAfter.UTC().Format(time.RFC3339)
	r.mu.Lock()
	r.info[host] = &renewalInfo{
		Host:         host,
		CertReady:    true,
		CertNotAfter: notAfter,
		LastTry:      time.Now().UTC().Format(time.RFC3339),
	}
	r.mu.Unlock()
	return notAfter, nil
}

// loop 周期性执行自动续签(阻塞, 在独立 goroutine 运行)。
func (r *renews) loop(cache *statusCache) {
	ticker := time.NewTicker(renewLoopEvery)
	defer ticker.Stop()
	for range ticker.C {
		r.autoRenew(cache)
	}
}

// autoRenew 对达到阈值的监控主机触发续签。
func (r *renews) autoRenew(cache *statusCache) {
	rc := r.renewalCfg()
	if !rc.Enabled || rc.Email == "" {
		return
	}
	reports, _, _ := cache.get()
	for _, rep := range reports {
		// 同一域名可能监控多个端口, 续签只对域名(ACME DNS-01)执行一次
		host := rep.Host
		if _, _, err := splitHostPortEntry(host); err != nil {
			continue // 非法条目(已在监控报告中标错), 不参与续签
		}
		if rep.Status == statusError || rep.DaysLeft > float64(rc.Days) {
			continue
		}
		// 本地新证书仍有效(超过阈值)则跳过, 避免重复签发触发 LE 速率限制
		r.mu.Lock()
		inf, ok := r.info[host]
		r.mu.Unlock()
		if ok && inf.CertReady {
			if t, err := time.Parse(time.RFC3339, inf.CertNotAfter); err == nil &&
				time.Until(t) > time.Duration(rc.Days+1)*24*time.Hour {
				continue
			}
		}
		fmt.Printf("[renew] %s 剩余 %.1f 天 <= %d 天, 开始续签...\n", reportKey(rep), rep.DaysLeft, rc.Days)
		notAfter, err := r.issue(host)
		if err != nil {
			fmt.Printf("[renew] %s 续签失败: %v\n", reportKey(rep), err)
			r.mu.Lock()
			r.info[host] = &renewalInfo{
				Host:      host,
				LastTry:   time.Now().UTC().Format(time.RFC3339),
				LastError: err.Error(),
			}
			r.mu.Unlock()
			continue
		}
		fmt.Printf("[renew] %s 续签成功, 新证书到期 %s\n", reportKey(rep), notAfter)
	}
}

// statusFor 合并当前证书检查结果与最近签发状态。
func (r *renews) statusFor(reports []certReport) []renewalInfo {
	rc := r.renewalCfg()
	out := make([]renewalInfo, 0, len(reports))
	seen := map[string]bool{}
	for _, rep := range reports {
		host := rep.Host
		due := false
		if _, _, err := splitHostPortEntry(host); err == nil {
			due = rep.Status != statusError && rep.DaysLeft <= float64(rc.Days)
		} else {
			host = "" // 非法条目: 不关联续签信息
		}
		inf := renewalInfo{
			Host:     rep.Host,
			Port:     rep.Port,
			DaysLeft: rep.DaysLeft,
			Due:      due,
		}
		if prev, ok := r.info[host]; ok && host != "" && !seen[host] {
			seen[host] = true
			inf.CertReady = prev.CertReady
			inf.CertNotAfter = prev.CertNotAfter
			inf.LastTry = prev.LastTry
			inf.LastError = prev.LastError
		}
		out = append(out, inf)
	}
	return out
}

// handleRenew POST /api/renew {"host"} — 手动续签(需 token, 仅限监控主机)。
func (r *renews) handleRenew(dep *deployer, w http.ResponseWriter, req *http.Request) {
	if !dep.authorize(req) {
		if dep.token == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "续签未启用: 请设置环境变量 MONITOR_DEPLOY_TOKEN")
		} else {
			writeJSONError(w, http.StatusUnauthorized, "部署 token 无效")
		}
		return
	}
	var body struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请提供合法 host")
		return
	}
	// 允许 host:port 形式(面板传的是监控条目), 续签只对域名部分签发
	host, _, err := splitHostPortEntry(body.Host)
	if err != nil || !r.hostRe.MatchString(host) {
		host = body.Host
	}
	host = strings.ToLower(host)
	hosts := r.monitoredHosts()
	monitored := false
	for _, h := range hosts {
		if h == host {
			monitored = true
			break
		}
	}
	if !monitored {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("仅支持监控主机: %s", strings.Join(hosts, ", ")))
		return
	}

	// 注意: 签发期间不持有 r.mu(ACME 耗时分钟级, 且 saveCert 内部自行加锁)
	notAfter, err := r.issue(host)
	if err != nil {
		r.mu.Lock()
		r.info[host] = &renewalInfo{
			Host:      host,
			LastTry:   time.Now().UTC().Format(time.RFC3339),
			LastError: err.Error(),
		}
		r.mu.Unlock()
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "not_after": notAfter})
}

var certFileNames = map[string]bool{"cert": true, "fullchain": true, "privkey": true}

// handleCertDownload GET /api/cert/{host}/{file} — 下载已签发证书文件(私钥需 token)。
func (r *renews) handleCertDownload(dep *deployer, w http.ResponseWriter, req *http.Request) {
	host := req.PathValue("host")
	file := req.PathValue("file")
	if !r.hostRe.MatchString(host) || !certFileNames[file] {
		http.NotFound(w, req)
		return
	}
	if file == "privkey" && !dep.authorize(req) {
		writeJSONError(w, http.StatusUnauthorized, "下载私钥需部署 token")
		return
	}
	b, err := os.ReadFile(filepath.Join(renewCertDir, host, file+".pem"))
	if err != nil {
		http.Error(w, "cert not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s-%s.pem\"", host, file))
	w.Write(b)
}
