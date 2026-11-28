// cutover_registry_test.go：MIG-1（工作项 wi-1795853890）「注册表与部署清单一致性」守卫。
//
// 覆盖验收标准里的两条收尾要求：
//   - 步骤 2「skill 网关 x-scan / like 的 addr 切到 social」：注册表快照里两条能力的 addr
//     必须就是 skillgateway 的切流目标常量（http://social:8010/x-scan、/like）；
//   - 步骤 4/5「停止并删除旧容器 + 更新注册表与文档」：旧服务从统一 compose 的**主链路**
//     include 移出（挂到 profiles: [legacy]，回滚时才拉得起来），且快照与网关口径逐条一致。
//
// 与 apps/core/internal/deploy 的既有测试同口径：从包目录向上找仓库根（含
// deploy/docker-compose.yml 的目录）；找不到时跳过（CI 只挂 apps/core 的场合）。
package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetee/ai-workflow/internal/skillgateway"
)

// monorepoRoot 从包目录向上找 monorepo 根。
func monorepoRoot(t *testing.T) string {
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
			t.Skip("仓库根不在测试可及范围（只挂载了 apps/core 模块），跳过注册表守卫")
		}
		dir = parent
	}
}

func TestRegistrySnapshotTargetsSocial(t *testing.T) {
	root := monorepoRoot(t)
	snap, err := LoadRegistrySnapshot(root)
	if err != nil {
		t.Fatalf("装载注册表快照: %v", err)
	}
	if err := snap.Validate(root); err != nil {
		t.Fatalf("注册表快照校验失败: %v", err)
	}

	for _, skill := range []string{skillgateway.SkillXScan, skillgateway.SkillLike} {
		want := skillgateway.CutoverTargetForSkill(skill)
		if got := snap.TargetAddrOf(skill); got != want {
			t.Fatalf("注册表里 %s 的 addr = %q，必须等于网关的切流目标 %q", skill, got, want)
		}
	}
	if snap.TargetAddrOf("generate") != "" {
		t.Fatal("非收口范围的 skill 不该出现在 MIG-1 快照的目标里")
	}
}

func TestRegistrySnapshotLegacyServices(t *testing.T) {
	root := monorepoRoot(t)
	snap, err := LoadRegistrySnapshot(root)
	if err != nil {
		t.Fatalf("装载注册表快照: %v", err)
	}

	services := snap.DecommissionPlanFromSnapshot()
	want := DefaultLegacyServices()
	if len(services) != len(want) {
		t.Fatalf("快照的下线清单 %d 条，与 DefaultLegacyServices 的 %d 条不一致: %+v",
			len(services), len(want), services)
	}
	bySkill := map[string]LegacyService{}
	for _, s := range services {
		bySkill[s.Skill] = s
	}
	for _, w := range want {
		got, ok := bySkill[w.Skill]
		if !ok {
			t.Fatalf("快照的下线清单缺少 skill %s（旧服务 %s）", w.Skill, w.Service)
		}
		if got.Service != w.Service || got.Addr != w.Addr {
			t.Fatalf("skill %s 的旧服务口径不一致：快照 %+v vs 网关 %+v", w.Skill, got, w)
		}
	}
}

// TestLegacyComposeMovedToLegacyProfile 钉住「旧服务不再进主链路」：
// deploy/docker-compose.yml 的 include 必须带 profiles: [legacy]，这样
// `docker compose up -d --build`（MIG-1 步骤 4 之后的全量启动）不会再拉起 pycraper / pyreplay，
// 而回滚（skill-cutover rollback）时 `docker compose --profile legacy up -d pycraper` 仍可用。
func TestLegacyComposeMovedToLegacyProfile(t *testing.T) {
	root := monorepoRoot(t)
	snap, err := LoadRegistrySnapshot(root)
	if err != nil {
		t.Fatalf("装载注册表快照: %v", err)
	}

	includes, err := IncludedComposeFiles(root)
	if err != nil {
		t.Fatalf("读 include 清单: %v", err)
	}
	included := map[string]bool{}
	for _, f := range includes {
		included[f] = true
	}

	for _, lg := range snap.Decommissioned {
		if !included[lg.Compose] {
			t.Fatalf("%s 已从 include 移出：旧服务 %s 的回滚路径断了（文件 %s）", lg.Compose, lg.Service, lg.Compose)
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(lg.Compose)))
		if err != nil {
			t.Fatalf("读 %s: %v", lg.Compose, err)
		}
		if !strings.Contains(string(raw), "profiles: [legacy]") {
			t.Fatalf("%s 必须挂 profiles: [legacy]：全量 up 不该再拉起已下线的 %s", lg.Compose, lg.Service)
		}
	}

	// social 必须在主链路里（收口后的实现由全量启动拉起）。
	if !included["deploy/compose/core/social.yml"] {
		t.Fatal("deploy/compose/core/social.yml 必须在 include 主链路里（x-scan / like 的新实现）")
	}
}
