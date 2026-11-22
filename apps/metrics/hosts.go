package main

// 域名管理 API: 在 MySQL 配置(monitor_config.hosts)上增删监控域名, 30s 内热加载生效。
//
//   - POST   /api/hosts        {"host": "new.example.com"} → 添加(需 token)
//   - DELETE /api/hosts/{host}                          → 删除(需 token)
//
// 校验: 域名格式(小写, 合法主机名); 添加去重; 删除后 hosts 不能为空。
// 注意: 新域名需已解析到本机且 443 有证书, 否则监控显示 ERROR;
// 自动续签要求域名在阿里云 DNS 下(见 renew.go)。

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

// validateHost 校验并规范化域名: 去空白/协议/路径, 转小写, 检查主机名格式。
func validateHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if i := strings.IndexAny(h, "/:"); i >= 0 {
		h = h[:i]
	}
	h = strings.ToLower(h)
	if h == "" {
		return "", errors.New("域名不能为空")
	}
	if len(h) > 253 || strings.Contains(h, " ") {
		return "", errors.New("域名格式无效: " + raw)
	}
	for _, part := range strings.Split(h, ".") {
		if part == "" || len(part) > 63 || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return "", errors.New("域名格式无效: " + raw)
		}
		for _, c := range part {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return "", errors.New("域名格式无效: " + raw)
			}
		}
	}
	if net.ParseIP(h) != nil {
		return "", errors.New("仅支持域名, 不支持 IP: " + raw)
	}
	return h, nil
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
	host, err := validateHost(req.Host)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := store.get()
	for _, h := range cfg.Hosts {
		if h == host {
			writeJSONError(w, http.StatusConflict, "域名已存在: "+host)
			return
		}
	}
	cfg.Hosts = append(append([]string{}, cfg.Hosts...), host)
	if err := store.put(cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"host":       host,
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
	host, err := validateHost(r.PathValue("host"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := store.get()
	next := make([]string, 0, len(cfg.Hosts))
	found := false
	for _, h := range cfg.Hosts {
		if h == host {
			found = true
			continue
		}
		next = append(next, h)
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "域名不存在: "+host)
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
