package main

// 域名管理 API: 在 MySQL 配置(monitor_config.hosts)上增删监控域名, 30s 内热加载生效。
//
//   - POST   /api/hosts        {"host": "new.example.com[:port]"} → 添加(需 token)
//   - DELETE /api/hosts/{host} → 删除(需 token; host 可含 :port, 需与添加时一致)
//
// 校验: 域名格式(小写, 合法主机名), 可选 :port 后缀(如 xiaobai.asyou.me:8443,
// 同一域名可监控多个端口); 添加去重; 删除后 hosts 不能为空。
// 注意: 新域名需已解析到本机且对应端口有 TLS 证书, 否则监控显示 ERROR;
// 自动续签要求域名在阿里云 DNS 下(见 renew.go; 续签只对域名签发, 与端口无关)。

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// validateHost 校验并规范化监控条目: 去空白/协议/路径, 转小写, 检查主机名格式;
// 允许 :port 后缀(host:8443), 无端口时端口部分为 0。
func validateHost(raw string) (host string, port int, err error) {
	h := strings.TrimSpace(raw)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if i := strings.IndexAny(h, "/"); i >= 0 {
		h = h[:i]
	}
	// 分离端口(仅当冒号后全为数字; IPv6 不支持)
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i+1:], ":") {
		p, perr := strconv.Atoi(h[i+1:])
		if perr != nil || p < 1 || p > 65535 {
			return "", 0, errors.New("域名格式无效: " + raw)
		}
		port = p
		h = h[:i]
	}
	h = strings.ToLower(h)
	if h == "" {
		return "", 0, errors.New("域名不能为空")
	}
	if len(h) > 253 || strings.Contains(h, " ") {
		return "", 0, errors.New("域名格式无效: " + raw)
	}
	for _, part := range strings.Split(h, ".") {
		if part == "" || len(part) > 63 || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return "", 0, errors.New("域名格式无效: " + raw)
		}
		for _, c := range part {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return "", 0, errors.New("域名格式无效: " + raw)
			}
		}
	}
	if net.ParseIP(h) != nil {
		return "", 0, errors.New("仅支持域名, 不支持 IP: " + raw)
	}
	return h, port, nil
}

// hostEntry 把校验后的 (host, port) 拼回存储条目: 无端口时只存域名。
func hostEntry(host string, port int) string {
	if port == 0 {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}

// handleHostAdd POST /api/hosts {"host": "..."} → {ok, host, hosts}
func handleHostAdd(store configStore, dep *deployer, w http.ResponseWriter, r *http.Request) {
	if !dep.authorize(r) {
		respondAuthError(dep, w)
		return
	}
	var req struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	host, port, err := validateHost(req.Host)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	entry := hostEntry(host, port)
	cfg := store.get()
	for _, h := range cfg.Hosts {
		if strings.EqualFold(h, entry) {
			writeJSONError(w, http.StatusConflict, "监控目标已存在: "+entry)
			return
		}
	}
	cfg.Hosts = append(append([]string{}, cfg.Hosts...), entry)
	if err := store.put(cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"host":       entry,
		"hosts":      store.get().Hosts,
		"updated_at": store.updatedAt().UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// handleHostDelete DELETE /api/hosts/{host} → {ok, hosts}
func handleHostDelete(store configStore, dep *deployer, w http.ResponseWriter, r *http.Request) {
	if !dep.authorize(r) {
		respondAuthError(dep, w)
		return
	}
	host, port, err := validateHost(r.PathValue("host"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	entry := hostEntry(host, port)
	cfg := store.get()
	next := make([]string, 0, len(cfg.Hosts))
	found := false
	for _, h := range cfg.Hosts {
		if strings.EqualFold(h, entry) {
			found = true
			continue
		}
		next = append(next, h)
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "监控目标不存在: "+entry)
		return
	}
	if len(next) == 0 {
		writeJSONError(w, http.StatusBadRequest, "hosts 不能为空, 至少保留一个域名")
		return
	}
	cfg.Hosts = next
	if err := store.put(cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"hosts":      store.get().Hosts,
		"updated_at": store.updatedAt().UTC().Format("2006-01-02T15:04:05Z"),
	})
}
