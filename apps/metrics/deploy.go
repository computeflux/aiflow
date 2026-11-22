package main

// 部署控制: 从监控面板重启/重编译其他应用。
//
// 执行方式: 监控容器持有 docker socket, 通过
//
//	docker run --rm --privileged --pid=host <helper> \
//	  nsenter -t 1 -m -u -i -n -p -- setpriv --reuid=<uid> --regid=<uid> --init-groups \
//	    env XDG_RUNTIME_DIR=/run/user/<uid> HOME=<home> bash -lc '<脚本> <子命令>'
//
// 进入宿主机命名空间后以部署用户(含完整补充组)执行各应用现有的部署脚本,
// 宿主机的 docker / node / systemctl --user 均可直接使用, 各应用无需任何修改。
//
// 安全: 该能力等价于宿主机 root 权限, 因此 MONITOR_DEPLOY_TOKEN 未设置时
// 部署接口直接禁用(503); 设置后所有 /api/deploy 请求必须携带正确 token。

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// deployEntry 是配置中的一个可部署应用: name 与 services[].name 对应,
// script 是相对 MONITOR_REPO 的部署脚本, actions 是允许的子命令白名单。
type deployEntry struct {
	Name    string   `json:"name"`
	Script  string   `json:"script"`
	Actions []string `json:"actions"`
}

var deployActionRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

type deployer struct {
	mu     sync.Mutex
	store  configStore
	repo   string
	uid    int
	home   string
	helper string
	token  string
}

func newDeployer(store configStore) *deployer {
	return &deployer{
		store:  store,
		repo:   envOr("MONITOR_REPO", "/home/wetee/work/cathome/workflow"),
		uid:    envInt("MONITOR_DEPLOY_UID", 1001),
		home:   envOr("MONITOR_DEPLOY_HOME", "/home/wetee"),
		helper: envOr("DEPLOY_HELPER_IMAGE", "alpine:3.21"),
		token:  os.Getenv("MONITOR_DEPLOY_TOKEN"),
	}
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

// entries 返回当前配置中的可部署应用列表。
func (d *deployer) entries() []deployEntry {
	return d.store.get().Deployables
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// handleDeployConfig GET /api/deploy/config → {enabled, deployables: [{name, script, actions}]}
func (d *deployer) handleDeployConfig(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name    string   `json:"name"`
		Script  string   `json:"script"`
		Actions []string `json:"actions"`
	}
	entries := d.entries()
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		items = append(items, item{Name: e.Name, Script: e.Script, Actions: e.Actions})
	}
	writeJSON(w, map[string]any{
		"enabled":     d.token != "",
		"deployables": items,
	})
}

type deployRequest struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// handleDeploy POST /api/deploy {"name","action"} → {ok, exit_code, duration_ms, output}
// authorize 检查部署 token(变更类接口共用: /api/deploy、/api/renew、私钥下载)。
// 静态 MONITOR_DEPLOY_TOKEN 或 auth JWT（admin 角色，JWKS 本地验签）均可。
func (d *deployer) authorize(r *http.Request) bool {
	if d.token != "" && d.checkToken(r) {
		return true
	}
	return authnAuthorized(r)
}

func (d *deployer) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if !d.authorize(r) {
		if d.token == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "部署未启用: 请设置环境变量 MONITOR_DEPLOY_TOKEN")
		} else {
			writeJSONError(w, http.StatusUnauthorized, "部署 token 无效")
		}
		return
	}
	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	var entry *deployEntry
	for i := range d.entries() {
		if d.entries()[i].Name == req.Name {
			entry = &d.entries()[i]
			break
		}
	}
	if entry == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("无可部署的应用: %s", req.Name))
		return
	}
	actionAllowed := false
	for _, a := range entry.Actions {
		if a == req.Action {
			actionAllowed = true
			break
		}
	}
	if !actionAllowed || !deployActionRe.MatchString(req.Action) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("应用 %s 不允许执行 %s", req.Name, req.Action))
		return
	}
	script, err := d.resolveScript(*entry)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	d.mu.Lock() // 同一时刻只允许一个部署任务
	defer d.mu.Unlock()

	cmdLine := fmt.Sprintf("cd %q && bash %q %s", d.repo, script, req.Action)
	args := []string{
		"run", "--rm", "--privileged", "--pid=host", d.helper,
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p",
		"--",
		"setpriv", "--reuid=" + strconv.Itoa(d.uid), "--regid=" + strconv.Itoa(d.uid), "--init-groups",
		"env", "XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(d.uid), "HOME=" + d.home,
		"bash", "-lc", cmdLine,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	start := time.Now()
	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		exitCode = -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	writeJSON(w, map[string]any{
		"ok":          runErr == nil,
		"exit_code":   exitCode,
		"duration_ms": time.Since(start).Milliseconds(),
		"output":      tailString(out.String(), 20000),
	})
}

// resolveScript 解析部署脚本绝对路径(必须位于仓库目录内且存在)。
func (d *deployer) resolveScript(e deployEntry) (string, error) {
	abs, err := filepath.Abs(filepath.Join(d.repo, e.Script))
	if err != nil {
		return "", err
	}
	root := d.repo + string(filepath.Separator)
	if abs != d.repo && !strings.HasPrefix(abs, root) {
		return "", errors.New("部署脚本必须在仓库目录内")
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return "", fmt.Errorf("部署脚本不存在: %s", e.Script)
	}
	return abs, nil
}

// checkToken 依次从 X-Deploy-Token 头、Authorization: Bearer、?token= 查询参数取值比对。
func (d *deployer) checkToken(r *http.Request) bool {
	t := r.Header.Get("X-Deploy-Token")
	if t == "" {
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, "Bearer ") {
			t = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if t == "" {
		t = r.URL.Query().Get("token")
	}
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(d.token)) == 1
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"error": msg})
}

// tailString 只保留输出末尾 n 字节(构建日志可能很长)。
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…[前文省略]…\n" + s[len(s)-n:]
}
