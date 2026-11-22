package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// serviceEntry 是配置中的一个监控服务(名称 + 健康检查地址)。
type serviceEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

const (
	serviceUp   = "UP"
	serviceDown = "DOWN"
)

// serviceReport 是单个服务的健康检查结果。
type serviceReport struct {
	Name      string  `json:"name"`
	URL       string  `json:"url"`
	Status    string  `json:"status"` // UP / DOWN
	LatencyMs float64 `json:"latency_ms,omitempty"`
	HTTPCode  int     `json:"http_code,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// statusCache 缓存最近一次检查结果, 避免频繁轮询造成重复 TLS 握手 / HTTP 探测。
// 每次请求时若缓存未过期则直接返回, 否则按当前配置重新检查(并行, 单次秒级)。
type statusCache struct {
	mu    sync.Mutex
	store configStore
	ttl   time.Duration

	certReports []certReport
	svcReports  []serviceReport
	at          time.Time
}

func newStatusCache(store configStore) *statusCache {
	return &statusCache{store: store, ttl: 60 * time.Second}
}

// get 返回缓存(若未过期)或重新检查的结果, 以及采集时间。
func (c *statusCache) get() ([]certReport, []serviceReport, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.certReports != nil && now.Sub(c.at) < c.ttl {
		return c.certReports, c.svcReports, c.at
	}
	// 持有锁执行检查: 检查本身秒级完成, 避免并发请求重复握手
	cfg := c.store.get()
	hosts := effectiveHosts(cfg)
	port := 443
	if cfg.Port > 0 {
		port = cfg.Port
	}
	warnDays := 30
	if cfg.WarnDays > 0 {
		warnDays = cfg.WarnDays
	}
	var services []serviceEntry
	for _, s := range cfg.Services {
		if s.URL == "" {
			continue
		}
		services = append(services, serviceEntry{Name: s.Name, URL: resolveServiceURL(s.URL)})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.certReports = checkAll(hosts, port, warnDays)
	}()
	go func() {
		defer wg.Done()
		c.svcReports = checkServices(services)
	}()
	wg.Wait()
	c.at = now
	return c.certReports, c.svcReports, c.at
}

// handleStatus GET /api/status → {"collected_at": ..., "hosts": [...], "services": [...]}
func (c *statusCache) handleStatus(w http.ResponseWriter, r *http.Request) {
	certs, services, at := c.get()
	writeJSON(w, map[string]any{
		"collected_at": at.UTC().Format(time.RFC3339),
		"hosts":        certs,
		"services":     services,
	})
}

// checkServices 并行探测所有服务的健康地址。
func checkServices(entries []serviceEntry) []serviceReport {
	reports := make([]serviceReport, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func(i int, e serviceEntry) {
			defer wg.Done()
			reports[i] = checkService(e)
		}(i, e)
	}
	wg.Wait()
	return reports
}

var svcHTTPClient = &http.Client{Timeout: 5 * time.Second}

// checkService 对服务地址发起 HTTP GET: 收到任意 HTTP 响应(含 4xx/5xx)
// 即视为 UP(进程存活可响应), 连接失败/超时视为 DOWN。
func checkService(e serviceEntry) serviceReport {
	r := serviceReport{Name: e.Name, URL: e.URL}
	start := time.Now()
	resp, err := svcHTTPClient.Get(e.URL)
	if err != nil {
		r.Status = serviceDown
		r.Error = err.Error()
		return r
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	ms := math.Round(float64(time.Since(start).Microseconds())/1000*10) / 10
	r.Status = serviceUp
	r.LatencyMs = ms
	r.HTTPCode = resp.StatusCode
	return r
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleStatic 托管前端静态文件(编译期内嵌产物 web/dist), SPA fallback 到 index.html。
func handleStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(embeddedDist, "web/dist")
	if err != nil {
		http.Error(w, "embedded frontend unavailable", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if b, err := fs.ReadFile(sub, name); err == nil {
		w.Header().Set("Content-Type", contentTypeFor(name))
		w.Write(b)
		return
	}
	// SPA fallback
	if b, err := fs.ReadFile(sub, "index.html"); err == nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write(b)
		return
	}
	http.Error(w, "frontend not found", http.StatusNotFound)
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"):
		return "application/javascript"
	case strings.HasSuffix(name, ".css"):
		return "text/css"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	default:
		return "text/html"
	}
}

// startHTTP 启动 HTTP 服务(阻塞直到出错返回):
//   - GET  /api/status        项目指标 JSON: 服务健康 + 证书状态 + 续签状态(前端轮询)
//   - GET/PUT /api/config     完整配置读写, 存 MySQL(需 token, 详见 dbconfig.go)
//   - POST   /api/hosts       添加监控域名(需 token, 详见 hosts.go)
//   - DELETE /api/hosts/{h}   删除监控域名(需 token, 详见 hosts.go)
//   - GET  /api/deploy/config 可部署应用列表(前端渲染按钮)
//   - POST /api/deploy        执行部署(需 token, 详见 deploy.go)
//   - POST /api/renew         手动续签证书(需 token, 详见 renew.go)
//   - GET  /api/cert/{h}/{f}  下载已签发证书(私钥需 token)
//   - GET  /healthz           健康检查
//   - GET  /                  前端静态页面(编译期内嵌 web/dist)
func startHTTP(store configStore, httpPort int) error {
	cache := newStatusCache(store)
	deploy := newDeployer(store)
	renew := newRenewer(store)
	store.startRefresh()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		handleConfigGet(store, deploy, w, r)
	})
	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		handleConfigPut(store, deploy, w, r)
	})
	mux.HandleFunc("POST /api/hosts", func(w http.ResponseWriter, r *http.Request) {
		handleHostAdd(store, deploy, w, r)
	})
	mux.HandleFunc("DELETE /api/hosts/{host}", func(w http.ResponseWriter, r *http.Request) {
		handleHostDelete(store, deploy, w, r)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		reports, svcs, at := cache.get()
		body := map[string]any{
			"collected_at": at.UTC().Format(time.RFC3339),
			"hosts":        reports,
			"services":     svcs,
		}
		body["renewals"] = renew.statusFor(reports)
		writeJSON(w, body)
	})
	mux.HandleFunc("GET /api/deploy/config", deploy.handleDeployConfig)
	mux.HandleFunc("POST /api/deploy", deploy.handleDeploy)
	mux.HandleFunc("POST /api/renew", func(w http.ResponseWriter, r *http.Request) {
		renew.handleRenew(deploy, w, r)
	})
	mux.HandleFunc("GET /api/cert/{host}/{file}", func(w http.ResponseWriter, r *http.Request) {
		renew.handleCertDownload(deploy, w, r)
	})
	go renew.loop(cache)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", handleStatic)

	addr := ":" + strconv.Itoa(httpPort)
	srv := &http.Server{
		Addr:        addr,
		Handler:     corsMiddleware(mux),
		ReadTimeout: 30 * time.Second,
		// 部署/续签可能耗时数分钟(Docker 构建、DNS-01 签发), 放宽写超时
		WriteTimeout: 20 * time.Minute,
	}
	cfg := store.get()
	fmt.Printf("[metrics] http server listening on %s (%d hosts, %d services, config: MySQL %s, api: /api/status)\n", addr, len(effectiveHosts(cfg)), len(cfg.Services), monitorConfigTable)
	return srv.ListenAndServe()
}
