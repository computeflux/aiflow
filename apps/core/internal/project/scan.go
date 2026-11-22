package project

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScanApps 扫描仓库 apps/ 下的目录（1 目录 = 1 项目）：识别 runtime、给默认镜像与描述。
// 规则：
//   - 跳过隐藏目录与无可识别清单（go.mod / pyproject.toml / uv.lock / requirements.txt / package.json）的目录；
//   - 全部项目一律**默认启用**（active=true）：2026-10-18 起不再有「core 系 / board 系人工维护红线 → 默认停用」——
//     目标显式选中时全部走自动进化链（coder → reviewer → 沙箱 → CI），人工只在**审批 + 合并 PR** 处把关
//     （红线不变：diff 绝不自动 merge 主分支）。要关某个项目的自动执行，用项目启停开关（active=false）。
//   - 默认镜像按 runtime：python → <name>:latest（apps 自建镜像约定）、go → golang:1.26、node → node:22。
func ScanApps(root string) ([]Project, error) {
	appsDir := filepath.Join(strings.TrimSuffix(root, "/"), "apps")
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(appsDir, name)
		runtime := DetectRuntime(dir)
		if runtime == "" {
			continue
		}
		out = append(out, Project{
			Name:         name,
			Dir:          "apps/" + name,
			Runtime:      runtime,
			Image:        DefaultImage(name, runtime),
			Description:  readDescription(dir),
			Active:       true,
			RegisteredAt: time.Now(),
		})
	}
	return out, nil
}

// DetectRuntime 识别项目 runtime（按清单文件，返回 "" 表示不是可识别项目）。
func DetectRuntime(dir string) string {
	switch {
	case fileExists(dir, "go.mod"):
		return "go"
	case fileExists(dir, "pyproject.toml"), fileExists(dir, "uv.lock"), fileExists(dir, "requirements.txt"):
		return "python"
	case fileExists(dir, "package.json"):
		return "node"
	}
	return ""
}

// DefaultImage 按 runtime 给默认沙箱镜像（须在 SANDBOX_ALLOWED_IMAGES 白名单内才能执行）。
func DefaultImage(name, runtime string) string {
	switch runtime {
	case "python":
		return name + ":latest" // apps 自建镜像约定：<dir>:latest（如 pycraper:latest / pyreplay:latest）
	case "go":
		return "golang:1.26"
	case "node":
		return "node:22"
	}
	return ""
}

// readDescription 读项目 README 首行作为描述（可选，读不到返回空）。
func readDescription(dir string) string {
	for _, f := range []string{"README.md", "README.MD", "readme.md"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if line != "" {
				return truncate(line, 200)
			}
		}
	}
	return ""
}

func fileExists(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !st.IsDir()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
