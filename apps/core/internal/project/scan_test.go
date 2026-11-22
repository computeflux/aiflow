package project

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanApps 锁定 apps/ 目录扫描规则：识别 runtime、跳过非项目目录、board* 默认停用。
func TestScanApps(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, files ...string) {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("apps/pycraper", "pyproject.toml", "uv.lock")
	mk("apps/mailer", "go.mod")
	mk("apps/minishop", "package.json")
	mk("apps/board-ops", "package.json")
	mk("apps/core", "go.mod")
	mk("apps/core-ui", "package.json")
	mk("apps/plain-dir")                                                           // 无可识别清单 → 跳过
	mk("apps/.hidden", "go.mod")                                                   // 隐藏目录 → 跳过
	_ = os.WriteFile(filepath.Join(root, "apps", "AGENTS.md"), []byte("x"), 0o644) // 文件 → 跳过

	got, err := ScanApps(root)
	if err != nil {
		t.Fatalf("ScanApps: %v", err)
	}
	byName := map[string]Project{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if len(got) != 6 {
		t.Fatalf("扫描到 %d 个项目（期望 6：pycraper/mailer/minishop/board-ops/core/core-ui）：%+v", len(got), byName)
	}
	if p := byName["pycraper"]; p.Runtime != "python" || p.Dir != "apps/pycraper" || p.Image != "pycraper:latest" || !p.Active {
		t.Errorf("pycraper 注册错误：%+v", p)
	}
	if p := byName["mailer"]; p.Runtime != "go" || p.Image != "golang:1.26" {
		t.Errorf("mailer 注册错误：%+v", p)
	}
	if p := byName["minishop"]; p.Runtime != "node" || p.Image != "node:22" {
		t.Errorf("minishop 注册错误：%+v", p)
	}
	// 2026-10-18 起不再有「core 系 / board 系人工维护 → 默认停用」：全部默认启用，
	// active 只作启停开关（用户可手动关）；自动执行的把关在审批 + 合并 PR。
	if p := byName["board-ops"]; !p.Active {
		t.Errorf("board-ops 应默认启用（不再有默认停用的人工维护红线）：%+v", p)
	}
	if p := byName["core"]; !p.Active {
		t.Errorf("core 应默认启用（不再有默认停用的人工维护红线）：%+v", p)
	}
	if p := byName["core-ui"]; !p.Active {
		t.Errorf("core-ui 应默认启用（不再有默认停用的人工维护红线）：%+v", p)
	}
	if _, ok := byName["plain-dir"]; ok {
		t.Errorf("无可识别清单的目录不应注册")
	}
}

// TestDefaultTestCmd 锁定沙箱默认测试命令（go / python / node）。
func TestDefaultTestCmd(t *testing.T) {
	if got := DefaultTestCmd("go"); len(got) == 0 || got[0] != "go" {
		t.Errorf("go 默认命令错误：%v", got)
	}
	if got := DefaultTestCmd("python"); len(got) != 3 || got[0] != "sh" {
		t.Errorf("python 默认命令错误：%v", got)
	}
	if got := DefaultTestCmd("node"); len(got) != 3 || got[0] != "sh" {
		t.Errorf("node 默认命令错误：%v", got)
	}
	if got := DefaultTestCmd("rust"); got != nil {
		t.Errorf("未知 runtime 应无默认命令：%v", got)
	}
	p := Project{Runtime: "go", TestCmd: "go test ./cmd/..."}
	if got := p.SandboxCmd(); len(got) != 3 || got[2] != "go test ./cmd/..." {
		t.Errorf("test_cmd 覆盖未生效：%v", got)
	}
}
