package deploy

// asset_scan_test.go：S5 软件资产画像（原 internal/asset，批次 G 并入 deploy 域）。

import (
	"context"
	"testing"
)

func TestScanRepo(t *testing.T) {
	s := NewScanner(repoRoot(t), nil) // nil db = 无已注册技能（接口维度仅路由/CLI）
	assets, err := s.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byName := map[string]SoftwareAsset{}
	for _, a := range assets {
		byName[a.Name] = a
	}

	// 关键资产存在（contract-deploy / pi-web 等嵌套独立 git 仓库被 .gitignore 忽略，
	// 不在 git worktree 里，CI 环境无法断言其存在；board 前端已合并为 core-ui 单容器四路径入口）
	for _, name := range []string{"workflow-api", "workflow-go-worker", "core-ui", "mailer", "generate-skill", "temporal", "health-patrol", "deployctl", "minishop", "postiz"} {
		if byName[name] == (SoftwareAsset{}) {
			t.Fatalf("missing asset %q", name)
		}
	}

	// 类型判定
	if byName["workflow-api"].Kind != "go-service" {
		t.Fatalf("workflow-api kind = %q", byName["workflow-api"].Kind)
	}
	if byName["core-ui"].Kind != "react-app" {
		t.Fatalf("core-ui kind = %q", byName["core-ui"].Kind)
	}
	if byName["deployctl"].Kind != "cli" {
		t.Fatalf("deployctl kind = %q", byName["deployctl"].Kind)
	}
	if byName["minishop"].Kind != "react-app" {
		t.Fatalf("minishop kind = %q", byName["minishop"].Kind)
	}

	// 纳管判定（红线；目录不存在时跳过——嵌套仓库在 worktree 中缺失）
	// 注：pi-web 跑在宿主（systemd 用户单元，非 compose），属不纳管清单，与 contract-deploy 等同类。
	for _, name := range []string{"workflow-api", "workflow-go-worker", "core-ui", "mailer", "temporal", "health-patrol"} {
		a := byName[name]
		if a == (SoftwareAsset{}) {
			continue // 目录不存在时跳过
		}
		if a.AutoManaged != 1 {
			t.Fatalf("%s should be managed", name)
		}
	}
	for _, name := range []string{"postiz", "contract-deploy", "tee-node-monitor", "c2c", "minishop", "actives", "pi-web"} {
		a := byName[name]
		if a == (SoftwareAsset{}) {
			continue // 目录不存在时跳过
		}
		if a.StartMethod != "external" || a.AutoManaged != 0 {
			t.Fatalf("%s should be external/unmanaged (method=%q managed=%d)", name, a.StartMethod, a.AutoManaged)
		}
	}

	// 端口（workflow-api 127.0.0.1:8001）
	api := byName["workflow-api"]
	if api.Ports == "[]" || api.Ports == "" {
		t.Fatalf("workflow-api ports empty: %q", api.Ports)
	}
	// owner 缺省 wetee
	if byName["workflow-api"].Owner != "wetee" {
		t.Fatalf("owner = %q", byName["workflow-api"].Owner)
	}
}
