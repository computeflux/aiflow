// cutover_sync_test.go：MIG-1「注册表同步」（cutover_sync.go）的行为单测。
//
// 钉住的是同步这一层的权限边界（每一条都对应一种「注册表同步顺手把生产改了」的事故形状）：
//   - 只建零流量 draft 版本：放量必须走 Stage（时长 + 质量护栏），同步抬流量 = 绕过护栏；
//   - 绝不写 skills.addr：基线 addr 只有 Complete/Rollback 能动，同步写它 = 一次注册全量切换；
//   - 幂等：同一份快照跑两次，第二次一条都不建（运维会反复跑）；
//   - 整批语义：任一 skill 对不上（未注册 / addr 漂移 / 无 live 基线）就一条都不写；
//   - dry-run 与真跑同判定：预演说会建的条数必须等于真跑创建的条数。
package migration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/skillgateway"
)

// mig1Snapshot 是内嵌的 MIG-1 快照（与 hacks/skill-registry/mig1-social.yaml 同口径；
// 仓库根可及时由 TestRegistrySnapshotTargetsSocial 比对两者一致）。
func mig1Snapshot() *RegistrySnapshot {
	return &RegistrySnapshot{
		Skills: []RegistrySkill{
			{Name: skillgateway.SkillXScan, Version: cutoverVer, Runtime: "go", Type: "service",
				Addr: skillgateway.SocialXScanAddr, Image: "social:latest", Container: "social"},
			{Name: skillgateway.SkillLike, Version: cutoverVer, Runtime: "go", Type: "service",
				Addr: skillgateway.SocialLikeAddr, Image: "social:latest", Container: "social"},
		},
		Decommissioned: []RegistryLegacy{
			{Service: "pycraper", Skill: skillgateway.SkillXScan, Addr: legacyXScan, Compose: "deploy/compose/core/pycraper.yml"},
			{Service: "pyreplay", Skill: skillgateway.SkillLike, Addr: legacyLike, Compose: "deploy/compose/core/pyreplay.yml"},
		},
	}
}

func countVersions(t *testing.T, f *cutFixture, name, version string) int {
	t.Helper()
	var n int64
	if err := f.db.Model(&db.SkillVersion{}).
		Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND version = ?", name, version).
		Count(&n).Error; err != nil {
		t.Fatalf("count versions %s/%s: %v", name, version, err)
	}
	return int(n)
}

func TestSyncRegistryCreatesZeroTrafficDraft(t *testing.T) {
	f := newCutFixture(t)
	ctx := context.Background()

	res, err := mig1Snapshot().SyncRegistry(ctx, f.db, false)
	if err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("应逐条给出 2 条结果，得到 %d", len(res.Changes))
	}
	for _, ch := range res.Changes {
		if ch.Action != SyncCreated {
			t.Fatalf("%s 的动作 = %q，期望 %q", ch.Skill, ch.Action, SyncCreated)
		}
		split, status := f.traffic(t, ch.Skill, ch.Version)
		if split != 0 || status != "draft" {
			t.Fatalf("%s v%s 必须是零流量 draft（放量只能由 Stage 做），得到 %d%%/%s",
				ch.Skill, ch.Version, split, status)
		}
		if got := f.addrOf(t, ch.Skill); got == ch.Addr {
			t.Fatalf("%s 的基线 addr 被同步改成了 %q：基线 addr 只有 Complete/Rollback 能动", ch.Skill, got)
		}
		var v db.SkillVersion
		if err := f.db.Where("skill_id = ? AND version = ?", ch.SkillID, ch.Version).First(&v).Error; err != nil {
			t.Fatalf("read back version: %v", err)
		}
		addr, err := configAddrStrict(v.Config)
		if err != nil || addr != ch.Addr {
			t.Fatalf("%s v%s 的 config.addr = %q（%v），期望 %q", ch.Skill, ch.Version, addr, err, ch.Addr)
		}
	}

	// 幂等：同一份快照再跑一次，一条都不新建、全部 unchanged。
	res2, err := mig1Snapshot().SyncRegistry(ctx, f.db, false)
	if err != nil {
		t.Fatalf("第二次 SyncRegistry: %v", err)
	}
	for _, ch := range res2.Changes {
		if ch.Action != SyncUnchanged {
			t.Fatalf("重复同步 %s 的动作 = %q，期望 %q", ch.Skill, ch.Action, SyncUnchanged)
		}
		if n := countVersions(t, f, ch.Skill, ch.Version); n != 1 {
			t.Fatalf("%s v%s 出现 %d 行版本（同步必须幂等）", ch.Skill, ch.Version, n)
		}
	}
}

func TestSyncRegistryDryRunWritesNothing(t *testing.T) {
	f := newCutFixture(t)
	ctx := context.Background()

	res, err := mig1Snapshot().SyncRegistry(ctx, f.db, true)
	if err != nil {
		t.Fatalf("SyncRegistry(dry-run): %v", err)
	}
	want := 0
	for _, ch := range res.Changes {
		if ch.Action != SyncWouldCrate {
			t.Fatalf("dry-run 的动作 = %q，期望 %q", ch.Action, SyncWouldCrate)
		}
		want++
	}
	if n := countVersions(t, f, skillgateway.SkillLike, cutoverVer); n != 0 {
		t.Fatal("dry-run 写了库")
	}

	if _, err := mig1Snapshot().SyncRegistry(ctx, f.db, false); err != nil {
		t.Fatalf("真跑 SyncRegistry: %v", err)
	}
	// 逐条比对：预演的每一条都必须被真跑创建出来（只数 like 一条会把「x-scan 没建」读成通过）。
	created := 0
	for _, ch := range res.Changes {
		if n := countVersions(t, f, ch.Skill, ch.Version); n != 1 {
			t.Fatalf("预演说要建 %s v%s，真跑之后库里有 %d 行", ch.Skill, ch.Version, n)
		}
		created++
	}
	if created != want {
		t.Fatalf("预演 %d 条、真跑 %d 条", want, created)
	}
}

func TestSyncRegistryRejectsWholeBatchAtomically(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, f *cutFixture)
		wantMsg string
	}{
		{
			name: "skill 未注册",
			mutate: func(t *testing.T, f *cutFixture) {
				if err := f.db.Where("name = ?", skillgateway.SkillLike).Delete(&db.Skill{}).Error; err != nil {
					t.Fatalf("delete skill: %v", err)
				}
			},
			wantMsg: "未注册",
		},
		{
			name: "同版本 addr 漂移",
			mutate: func(t *testing.T, f *cutFixture) {
				cfg, _ := json.Marshal(map[string]any{"addr": "http://social:8010/like-v2"})
				if err := f.db.Create(&db.SkillVersion{
					SkillID: skillIDOf(t, f, skillgateway.SkillLike), Version: cutoverVer,
					Image: "social:latest", TrafficSplit: 0, Status: "draft", Config: string(cfg),
				}).Error; err != nil {
					t.Fatalf("seed version: %v", err)
				}
			},
			wantMsg: "与快照",
		},
		{
			name: "无 live 基线可继承 config",
			mutate: func(t *testing.T, f *cutFixture) {
				if err := f.db.Model(&db.SkillVersion{}).
					Where("skill_id = ? AND version = ?", skillIDOf(t, f, skillgateway.SkillLike), baselineVer).
					Update("traffic_split", 0).Error; err != nil {
					t.Fatalf("retire baseline: %v", err)
				}
			},
			wantMsg: "无 live 基线",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCutFixture(t)
			tc.mutate(t, f)

			res, err := mig1Snapshot().SyncRegistry(context.Background(), f.db, false)
			if err == nil {
				t.Fatal("有拒绝项时必须整批失败")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("拒绝原因要能告诉运维差在哪（含 %q），得到 %v", tc.wantMsg, err)
			}
			if res == nil || len(res.Changes) == 0 {
				t.Fatal("被拒时也要把逐条结果带回去（board 要显示哪一条被拒）")
			}
			// 整批不写：另一条本来可以建的 skill 也必须没有版本行。
			if n := countVersions(t, f, skillgateway.SkillXScan, cutoverVer); n != 0 {
				t.Fatalf("整批拒绝时 x-scan 也被写进了库（%d 行）", n)
			}
		})
	}
}

// TestSyncRegistryTargetMatchesCutoverTarget 钉住「同步写入的 addr」与「切流登记的 target」
// 是同一份口径：两条路径各写一处，收尾后 skills.addr 与版本 config.addr 就会指向两个实现，
// 回滚路径（按 skills.addr 恢复基线）与灰度路由（按版本 config.addr）互相看不见对方。
func TestSyncRegistryTargetMatchesCutoverTarget(t *testing.T) {
	snap := mig1Snapshot()
	for _, sk := range snap.Skills {
		want := skillgateway.CutoverTargetForSkill(sk.Name)
		if got := snap.TargetAddrOf(sk.Name); got != want {
			t.Fatalf("快照里 %s 的 addr = %q，与切流目标 %q 不同", sk.Name, got, want)
		}
	}
}

func skillIDOf(t *testing.T, f *cutFixture, name string) uint {
	t.Helper()
	var s db.Skill
	if err := f.db.Where("name = ?", name).First(&s).Error; err != nil {
		t.Fatalf("skill %s: %v", name, err)
	}
	return s.ID
}
