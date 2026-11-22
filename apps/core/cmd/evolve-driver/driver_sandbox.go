package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/dockerapi"
	"github.com/wetee/ai-workflow/internal/project"
)

type sandboxResult struct {
	ExitCode   int    `json:"exit_code"`
	StdoutTail string `json:"stdout_tail"`
	StderrTail string `json:"stderr_tail"`
	DurationMs int64  `json:"duration_ms"`

	cmd []string // 本次沙箱命令（内部用：判定 pytest exit 5 = 未收集到用例）
}

// isPytestNoTests 判定“pytest 未收集到用例”（exit 5，与 ci-runner.sh 同口径）：
// 手工脚本型项目（tests/*.py 非 pytest 用例）不算失败，留痕放行；其它非零仍拦截。
func isPytestNoTests(cmd []string, exitCode int) bool {
	if exitCode != 5 {
		return false
	}
	for _, c := range cmd {
		if strings.Contains(c, "pytest") {
			return true
		}
	}
	return false
}

// runSandbox 调 sandbox-runner 跑目标项目单测（与 workitem.IsRunnableTarget 门禁对齐，S9.8 §7.10 只按项目识别）：
// 主目标（多值 target_name 第一个）须在 projects 表注册且启用 → 用项目配置镜像 + 项目目录 apps/<dir>，
// 命令按 runtime/test_cmd 默认（S9 §7.8 项目注册）；未注册/未启用/无命令/无镜像 → skipped=true（跳过留痕，
// 由 CI 全量兜底）；只有 runner 通信/运行错误才返回 error（打回人工）。
func (d *driver) runSandbox(task *db.EvolveTask) (*sandboxResult, bool, error) {
	img, repoMount, cmd, skipped, err := d.resolveSandboxTarget(task)
	if err != nil {
		return nil, false, err
	}
	if skipped {
		return nil, true, nil
	}
	body := map[string]any{
		"image":       img,
		"repo_mount":  repoMount,
		"cmd":         cmd,
		"timeout_sec": 600,
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(d.cfg.SandboxRunnerURL+"/sandbox/run", "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, false, fmt.Errorf("sandbox runner: %v", err)
	}
	defer resp.Body.Close()
	// 先读原文再判状态码（2026-10-19 卡 #101 实测修正）：runner 失败时回的是**纯文本**
	// （http.Error(w, "sandbox run failed: ...")，如 docker No such image），旧代码先 json.Decode →
	// 真实原因被吞成 "invalid character 's' looking for beginning of value"，完全无法定位。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = "(空响应)"
		}
		return nil, false, fmt.Errorf("sandbox HTTP %d: %s", resp.StatusCode, msg)
	}
	var out sandboxResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("sandbox response: %v（HTTP %d 原文：%s）", err, resp.StatusCode, tail(strings.TrimSpace(string(raw)), 200))
	}
	out.cmd = cmd // 内部留用（判定 pytest exit 5 = 未收集到用例，同 ci-runner 口径）
	d.store.Log(d.ctx(), "evolve-driver", "sandbox_done", task.TaskNo, map[string]any{
		"image": img, "exit_code": out.ExitCode, "duration_ms": out.DurationMs,
	})
	return &out, false, nil
}

// resolveSandboxTarget 解析目标（项目）为（镜像, 挂载目录, 命令）：apps/<dir> 1:1 对应项目名
// （S9 §7.8 项目注册表；S9.8 §7.10 只按项目识别）。
// 配置性问题（未注册/无命令/无镜像/不在白名单）→ skipped=true（跳过留痕，CI 兜底）；
// 只有 DB 查询本身出错才返回 error。
func (d *driver) resolveSandboxTarget(task *db.EvolveTask) (string, string, []string, bool, error) {
	var proj project.Project
	if err := d.db.Table("projects").Where("name = ? AND active = ?", task.TargetName, true).First(&proj).Error; err != nil {
		return "", "", nil, true, nil // 未注册 → 跳过（留痕由调用方打）
	}
	cmd := proj.SandboxCmd()
	if len(cmd) == 0 {
		return "", "", nil, true, nil // 无默认测试命令 → 跳过
	}
	if proj.Image == "" {
		return "", "", nil, true, nil // 未配置沙箱镜像 → 跳过
	}
	if !d.imageAllowed(proj.Image) {
		return "", "", nil, true, nil // 镜像不在白名单 → 跳过
	}
	mount := d.cfg.EvolveCacheDir + "/" + task.TaskNo + "/" + proj.Dir
	return proj.Image, mount, cmd, false, nil
}

// imageAllowed 镜像是否在白名单（支持 `repo:*` tag 通配，同 sandbox-runner 口径；白名单未配置时放行由 runner 拒绝）。
func (d *driver) imageAllowed(img string) bool {
	entries := sandboxImagesFromEnv()
	if len(entries) == 0 {
		return true // 白名单未配 → 放行，由 sandbox-runner 拒绝
	}
	return sandboximgAllowed(entries, img)
}

// ---- 镜像构建（灰度真跑新代码，2026-10-08）----

// versionName 返回该任务的版本名（evidence.version 优先，缺省 evolve-<task_no>，与审批 payload 同口径）。
func (d *driver) versionName(task *db.EvolveTask) string {
	ev := parseEvidence(task.Evidence)
	if v, _ := ev["version"].(string); v != "" {
		return v
	}
	return "evolve-" + task.TaskNo
}

// targetAppDir 解析目标（项目）对应的 apps/ 目录名（S9.8 §7.10 只按项目识别）。
func (d *driver) targetAppDir(task *db.EvolveTask) (string, error) {
	var proj project.Project
	if err := d.db.Table("projects").Where("name = ? AND active = ?", task.TargetName, true).First(&proj).Error; err == nil {
		return proj.Name, nil
	}
	return "", fmt.Errorf("目标 %s 未注册（无法确定 apps/ 目录）", task.TargetName)
}

// appAIFlow 读 apps/<app>/.aiflow（应用级流程配置，key: value 行格式），返回键值对。
// 文件不存在 / 解析失败返回空 map（不阻断，按默认流程走）。
// 例：image_build: skip → 跳过 docker 镜像构建（tee-node 等 TEE 进程走宿主部署）。
func appAIFlow(worktree, appDir string) map[string]string {
	return parseAIFlowFile(filepath.Join(worktree, "apps", appDir, ".aiflow"))
}

// parseAIFlowFile 解析 .aiflow（key: value 行格式；# 注释/空行忽略）。
func parseAIFlowFile(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// buildTargetImage 从 worktree 构建目标镜像 <app>:<version>（灰度/版本容器跑真实新代码）。
// 用 EVOLVE_BUILDER_IMAGE（deployctl 镜像：docker CLI + compose）一次性容器，
// 挂宿主 docker.sock + worktree，跑 docker build。
// 跳过条件（留痕，灰度回退继承 live 镜像）：
//   - apps/<app>/.aiflow 声明 image_build: skip（如 tee-node：TEE 进程走宿主部署，
//     且 Dockerfile 打包 ego 预编译产物，worktree 缺文件无法 docker build）；
//   - 无 Dockerfile。
//
// 预构建（.aiflow prebuild: <脚本>）：Dockerfile 打包预编译产物的项目，
// docker build 前先在 prebuild_image 容器里跑脚本（默认 BuilderImage；需 bash + 工具链）。
func (d *driver) buildTargetImage(task *db.EvolveTask) (string, error) {
	appDir, err := d.targetAppDir(task)
	if err != nil {
		log.Printf("evolve %s: %v（跳过镜像构建）", task.TaskNo, err)
		d.store.Log(d.ctx(), "evolve-driver", "image_build_skipped", task.TaskNo, map[string]any{"reason": err.Error()})
		return "", nil
	}
	worktree := d.cfg.RepoDir + "/" + d.cfg.EvolveCacheDir + "/" + task.TaskNo
	aiflow := appAIFlow(worktree, appDir)
	// image_build: skip → 跳过 docker 镜像构建
	if aiflow["image_build"] == "skip" {
		log.Printf("evolve %s: apps/%s .aiflow 声明 image_build: skip，跳过镜像构建（灰度继承 live 镜像）", task.TaskNo, appDir)
		d.store.Log(d.ctx(), "evolve-driver", "image_build_skipped", task.TaskNo, map[string]any{"app": appDir, "reason": "aiflow image_build: skip"})
		return "", nil
	}
	if _, err := os.Stat(worktree + "/apps/" + appDir + "/Dockerfile"); err != nil {
		log.Printf("evolve %s: apps/%s 无 Dockerfile，跳过镜像构建（灰度继承 live 镜像）", task.TaskNo, appDir)
		d.store.Log(d.ctx(), "evolve-driver", "image_build_skipped", task.TaskNo, map[string]any{"app": appDir, "reason": "no Dockerfile"})
		return "", nil
	}
	image := appDir + ":" + d.versionName(task)
	// 挂载源必须是**宿主**路径（docker daemon 在宿主侧解析 bind source）
	hostWorktree := strings.TrimSuffix(d.cfg.HostRepo, "/") + "/" + d.cfg.EvolveCacheDir + "/" + task.TaskNo
	// 预构建（.aiflow prebuild: <脚本，相对 apps/<app>/）：docker build 前先编译产物。
	// 容器镜像取 prebuild_image（默认 BuilderImage）；worktree 可写（产物落 worktree）。
	if script := aiflow["prebuild"]; script != "" {
		pbImage := aiflow["prebuild_image"]
		if pbImage == "" {
			pbImage = d.cfg.BuilderImage
		}
		log.Printf("evolve %s: 预构建 apps/%s（%s，image=%s）", task.TaskNo, appDir, script, pbImage)
		pb, err := d.docker.RunContainer(d.ctx(), dockerapi.ContainerSpec{
			Name:  "prebuild-" + task.TaskNo,
			Image: pbImage,
			Cmd:   []string{"bash", "/work/apps/" + appDir + "/" + script},
			User:  "1001:137",
			Env:   []string{"HOME=/tmp"},
			Mounts: []dockerapi.Mount{
				{Source: hostWorktree, Destination: "/work"},
			},
			Tmpfs:       []string{"/tmp"},
			NetworkMode: "default",
			Timeout:     30 * time.Minute,
		})
		if err != nil {
			return "", fmt.Errorf("prebuild 容器: %v", err)
		}
		if pb.ExitCode != 0 {
			return "", fmt.Errorf("prebuild 失败 exit=%d: %s", pb.ExitCode, tail(pb.Stderr, 400))
		}
	}
	log.Printf("evolve %s: 构建目标镜像 %s（worktree=%s）", task.TaskNo, image, hostWorktree)
	res, err := d.docker.RunContainer(d.ctx(), dockerapi.ContainerSpec{
		Name:  "build-" + task.TaskNo,
		Image: d.cfg.BuilderImage,
		// 构建上下文 = **app 目录**（2026-11-03 修正）。
		// 仓库里所有 app 的 Dockerfile 都是按 app 目录写的（compose 里也是
		// `context: ../../../apps/<app>`）：`COPY go.mod ./` / `COPY browser/browser_version.txt`。
		// 之前传 `/work`（仓库根）→ Dockerfile 里的相对路径全错位，
		// 典型报错：`COPY browser/browser_version.txt ... file not found in build context`。
		Cmd: []string{"docker", "build", "-f", "/work/apps/" + appDir + "/Dockerfile", "-t", image, "/work/apps/" + appDir},
		// 非 root 运行（同上：避免在 worktree 里留下 root 属主文件；经 docker.sock 构建仍需 docker 组）
		User: "1001:137",
		// docker CLI 需要可写的 HOME（config/cache）；不设会尝试写 /.docker → permission denied
		Env: []string{"HOME=/tmp", "DOCKER_CONFIG=/tmp/.docker"},
		// worktree（宿主机路径）+ 宿主 docker.sock（CLI 经 socket 连宿主 daemon；构建上下文由 CLI 打包上传）
		Mounts: []dockerapi.Mount{
			{Source: hostWorktree, Destination: "/work"},
			{Source: "/run/docker.sock", Destination: "/run/docker.sock"},
		},
		NetworkMode: "default",
		Timeout:     30 * time.Minute,
	})
	if err != nil {
		return "", fmt.Errorf("build 容器: %v", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("docker build 失败 exit=%d: %s", res.ExitCode, tail(res.Stderr, 400))
	}
	d.store.Log(d.ctx(), "evolve-driver", "image_built", task.TaskNo, map[string]any{"image": image, "app": appDir})
	return image, nil
}
