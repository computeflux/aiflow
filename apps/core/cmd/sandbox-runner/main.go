// Package main is the sandbox-runner binary.
//
// sandbox-runner：S1 沙箱执行器（docs/plan/S1-进化闭环.md §3.6）。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/wetee/ai-workflow/internal/dockerapi"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	Port             string `env:"SANDBOX_PORT" envDefault:"8123"`
	DockerHost       string `env:"SANDBOX_DOCKER_HOST" envDefault:"tcp://sandbox-dind:2375"`
	AllowedImages    string `env:"SANDBOX_ALLOWED_IMAGES" envDefault:""`
	RepoPrefix       string `env:"SANDBOX_REPO_PREFIX" envDefault:"/work"`
	MaxTimeoutSec    int    `env:"SANDBOX_MAX_TIMEOUT_SEC" envDefault:"1800"`
	OutputLimitBytes int    `env:"SANDBOX_OUTPUT_LIMIT" envDefault:"65536"`
}

type runRequest struct {
	Image      string            `json:"image"`
	RepoMount  string            `json:"repo_mount"` // 相对仓库根（如 cache/evolve/EV-.../apps/pyreplay）
	Cmd        []string          `json:"cmd"`
	TimeoutSec int               `json:"timeout_sec"`
	Env        map[string]string `json:"env"`
}

type runResponse struct {
	ExitCode   int    `json:"exit_code"`
	StdoutTail string `json:"stdout_tail"`
	StderrTail string `json:"stderr_tail"`
	DurationMs int64  `json:"duration_ms"`
}

func main() {
	_ = godotenv.Load()
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if cfg.AllowedImages == "" {
		log.Fatalf("SANDBOX_ALLOWED_IMAGES 未设置（白名单镜像，逗号分隔；支持 repo:* tag 通配）")
	}
	allowed := sandboximgSplit(cfg.AllowedImages)
	client, err := dockerapi.New(cfg.DockerHost)
	if err != nil {
		log.Fatalf("docker client: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/sandbox/run", func(w http.ResponseWriter, r *http.Request) {
		handleRun(w, r, cfg, allowed, client)
	})
	// 仅内部网络（宿主端口不发布），须绑 0.0.0.0 供 evolve-driver 容器访问
	addr := "0.0.0.0:" + cfg.Port
	log.Printf("sandbox-runner listening on %s (dind=%s, images=%v)", addr, cfg.DockerHost, cfg.AllowedImages)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func handleRun(w http.ResponseWriter, r *http.Request, cfg *Config, allowed []string, client *dockerapi.Client) {
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !sandboximgAllowed(allowed, req.Image) {
		log.Printf("sandbox rejected: image=%s 不在 SANDBOX_ALLOWED_IMAGES=%v", req.Image, allowed)
		http.Error(w, fmt.Sprintf("image not in SANDBOX_ALLOWED_IMAGES: %s（支持 repo:* tag 通配）", req.Image), http.StatusForbidden)
		return
	}
	if len(req.Cmd) == 0 {
		http.Error(w, "cmd required", http.StatusBadRequest)
		return
	}
	if req.RepoMount == "" {
		http.Error(w, "repo_mount required", http.StatusBadRequest)
		return
	}
	// 路径穿越防护：repo_mount 不得含 .. 或绝对路径
	if strings.Contains(req.RepoMount, "..") || strings.HasPrefix(req.RepoMount, "/") {
		http.Error(w, "repo_mount must be relative (no ..)", http.StatusBadRequest)
		return
	}
	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	if timeout > time.Duration(cfg.MaxTimeoutSec)*time.Second {
		timeout = time.Duration(cfg.MaxTimeoutSec) * time.Second
	}

	// env 白名单键：仅允许覆盖 MYSQL_*（测试需要连库）
	var envList []string
	for k, v := range req.Env {
		if strings.HasPrefix(k, "MYSQL_") {
			envList = append(envList, k+"="+v)
		}
	}
	// 容器内仓库根 = dind 挂载点（SANDBOX_REPO_PREFIX）+ 相对路径
	mountSource := strings.TrimSuffix(cfg.RepoPrefix, "/") + "/" + req.RepoMount

	start := time.Now()
	res, err := client.RunContainer(r.Context(), dockerapi.ContainerSpec{
		Name:        fmt.Sprintf("sandbox-%d", time.Now().UnixNano()),
		Image:       req.Image,
		Cmd:         req.Cmd,
		Env:         envList,
		Workdir:     "/work",
		Mounts:      []dockerapi.Mount{{Source: mountSource, Destination: "/work", ReadOnly: true}},
		NetworkMode: "none",  // 写死：沙箱无网络
		Memory:      1 << 30, // 1g
		CPUs:        2,
		PidsLimit:   256,
		ReadOnly:    true,
		Tmpfs:       []string{"/tmp", "/root/.cache", "/root/.crawl4ai"},
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		Timeout:     timeout,
	})
	if err != nil {
		// 失败必须进日志（否则 driver 只拿到纯文本响应，runner 侧无任何痕迹，2026-10-19 卡 #101 实测）：
		// 典型如镜像标签缺失（docker No such image: pycraper:latest）。
		log.Printf("sandbox run failed: image=%s mount=%s: %v", req.Image, req.RepoMount, err)
		http.Error(w, "sandbox run failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runResponse{
		ExitCode:   res.ExitCode,
		StdoutTail: truncate(res.Stdout, cfg.OutputLimitBytes),
		StderrTail: truncate(res.Stderr, cfg.OutputLimitBytes),
		DurationMs: time.Since(start).Milliseconds(),
	})
}

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n... [truncated, %d bytes total]", len(s))
}
