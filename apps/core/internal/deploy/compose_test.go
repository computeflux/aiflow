package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot 从包目录向上找 monorepo 根（含 deploy/docker-compose.yml 的目录；
// 2026-10-10 仓库重组后 Go 模块在 apps/core，deploy/ 在 monorepo 根）。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "docker-compose.yml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}

// externalSet 不纳管红线（复用包级 externalComposeServices，避免两份不同步）。
var externalSet = externalComposeServices

func TestLoadProject(t *testing.T) {
	p, err := LoadProject(repoRoot(t))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if len(p.Services) < 30 {
		t.Fatalf("expected >= 30 services, got %d", len(p.Services))
	}
	// 关键服务存在（board 前端已合并为 core-ui 单容器四路径入口 /ops + /product + /content + /email）
	for _, name := range []string{"workflow-api", "workflow-go-worker", "core-ui", "temporal", "mysql", "health-patrol", "deployctl"} {
		if p.Services[name] == nil {
			t.Fatalf("missing service %q", name)
		}
	}
	// 容器名 ≠ 服务名（mysql → workflow-mysql）
	if p.Services["mysql"].ContainerName != "workflow-mysql" {
		t.Fatalf("mysql container name = %q", p.Services["mysql"].ContainerName)
	}
	// 一次性容器判定
	for _, name := range []string{"temporal-admin-tools", "temporal-create-namespace", "synapse-init"} {
		if !p.Services[name].OneShot {
			t.Fatalf("%s should be one-shot", name)
		}
	}
	if p.Services["workflow-api"].OneShot {
		t.Fatal("workflow-api should not be one-shot")
	}
	// 端口解析
	api := p.Services["workflow-api"]
	if len(api.Ports) != 1 || api.Ports[0].Host != "127.0.0.1" || api.Ports[0].Container != "8001" {
		t.Fatalf("workflow-api ports = %+v", api.Ports)
	}
	// healthcheck HTTP 端点推导（core-ui 的 wget 探针，/ops/ 入口）
	if p.Services["core-ui"].HealthURL != "http://127.0.0.1:80/ops/" {
		t.Fatalf("core-ui health url = %q", p.Services["core-ui"].HealthURL)
	}
	// tools profile
	if !p.Services["deployctl"].IsTools() {
		t.Fatal("deployctl should be tools profile")
	}
}

func TestTopoOrder(t *testing.T) {
	p, err := LoadProject(repoRoot(t))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	order, err := p.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	if len(order) != len(p.Services) {
		t.Fatalf("order len %d != services %d", len(order), len(p.Services))
	}
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	// 依赖序：依赖者必须在其依赖之前
	for _, svc := range p.Services {
		for dep := range svc.DependsOn {
			if pos[svc.Name] < pos[dep] {
				t.Fatalf("%s (pos %d) before its dep %s (pos %d)", svc.Name, pos[svc.Name], dep, pos[dep])
			}
		}
	}
	// 基线序：infra 在 core 之前（temporal 先于 workflow-api）
	if pos["temporal"] > pos["workflow-api"] {
		t.Fatal("temporal should be before workflow-api")
	}
}

func TestGeneratePlan(t *testing.T) {
	p, err := LoadProject(repoRoot(t))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	// 空 managed = 全部纳管
	plan, err := GeneratePlan(p, nil, 30)
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("no steps")
	}
	// external 资产不进计划
	managed := map[string]bool{}
	for name := range p.Services {
		managed[name] = !externalSet[name]
	}
	plan2, err := GeneratePlan(p, managed, 30)
	if err != nil {
		t.Fatalf("GeneratePlan managed: %v", err)
	}
	for _, st := range plan2.Steps {
		if externalSet[st.Target] {
			t.Fatalf("external asset %s in plan", st.Target)
		}
	}
	// 计划可序列化（存 deploy_plans.plan）
	if _, err := UnmarshalPlan(mustJSONPlan(plan)); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
}

func mustJSONPlan(p *Plan) string {
	b, err := p.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return string(b)
}
