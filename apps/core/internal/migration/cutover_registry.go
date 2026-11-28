// cutover_registry.go：MIG-1（工作项 wi-1795853890）的注册表快照装载与部署清单核对。
//
// M4-1 把切流状态机建在数据库里（skill_cutover / skill_versions），本文件补上仓库侧的
// 同一份事实：apps/core/hacks/skill-registry/mig1-social.yaml 记录「x-scan / like 收口到
// social 之后，注册表应该长什么样」，以及旧实现下线后的 compose 归属。
//
// 为什么要把 YAML 装进 Go 侧并让单测读 deploy/docker-compose.yml（步骤 5「更新注册表」）：
//   - addr 只写在 MySQL 时，仓库里没有任何一处能回答「x-scan / like 现在指向谁」，下一张卡
//     与运维只能靠人记；快照进仓库 + 进测试，就是让这份事实可被机器核对；
//   - 两侧一致性（快照 addr ↔ skillgateway 的常量、快照 decommissioned ↔ compose 的 include）
//     一旦漂移，就是「文档说下线了、部署里还挂着」这种最坏的静默漂移。守卫单测把它变成红灯。
package migration

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/skillgateway"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// RegistrySnapshotPath 是 MIG-1 注册表快照（相对仓库根）。
const RegistrySnapshotPath = "apps/core/hacks/skill-registry/mig1-social.yaml"

// RegistrySnapshot 是注册表快照的解析结果。
type RegistrySnapshot struct {
	// Skills 是收口后仍在 skill 网关里的能力（本卡 = x-scan / like，addr 指向 social）。
	Skills []RegistrySkill `yaml:"skills"`
	// Decommissioned 是已下线的旧实现及其 compose 文件（保留文件本体供回滚）。
	Decommissioned []RegistryLegacy `yaml:"decommissioned"`
}

// RegistrySkill 是快照里的一条 skill 注册项。
type RegistrySkill struct {
	Name       string        `yaml:"name"`
	Version    string        `yaml:"version"`
	Runtime    string        `yaml:"runtime"`
	Type       string        `yaml:"type"`
	Addr       string        `yaml:"addr"`
	Image      string        `yaml:"image"`
	Container  string        `yaml:"container"`
	Limits     RegistryLimit `yaml:"limits"`
	Entrypoint string        `yaml:"entrypoint"`
}

// RegistryLimit 是 skill 的运行约束（与 skillgateway.Limits 同字段名）。
type RegistryLimit struct {
	TimeoutSec     int `yaml:"timeout_sec"`
	RatePerMin     int `yaml:"rate_per_min"`
	MaxConcurrency int `yaml:"max_concurrency"`
}

// RegistryLegacy 是一个下线的旧实现。
type RegistryLegacy struct {
	Service string `yaml:"service"`
	Skill   string `yaml:"skill"`
	Addr    string `yaml:"addr"`
	Compose string `yaml:"compose"`
	Note    string `yaml:"note"`
}

// LoadRegistrySnapshot 读取并解析注册表快照（repoRoot = 仓库根绝对路径）。
func LoadRegistrySnapshot(repoRoot string) (*RegistrySnapshot, error) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(RegistrySnapshotPath)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", RegistrySnapshotPath, err)
	}
	var snap RegistrySnapshot
	if err := yaml.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", RegistrySnapshotPath, err)
	}
	return &snap, nil
}

// TargetAddrOf 返回快照里某 skill 的目标 addr（不存在返回空串）。
func (s *RegistrySnapshot) TargetAddrOf(skillName string) string {
	for _, sk := range s.Skills {
		if sk.Name == skillName {
			return strings.TrimSuffix(strings.TrimSpace(sk.Addr), "/")
		}
	}
	return ""
}

// Validate 校验快照自洽，并逐条核对它引用的 compose 文件在仓库里存在。
//
// 校验口径与运行期护栏同源（见 cutover.go / decommission.go）：
//   - 每条 skill 必须是 service 型 + addr 指向 social（切流目标只有这一处来源）；
//   - 每条 decommissioned 必须能对上 skillgateway 的旧服务口径（LegacyServiceForSkill），
//     两侧服务名/addr 一致 —— 否则「下线清单」与「切流回滚要恢复的服务」就是两回事。
func (s *RegistrySnapshot) Validate(repoRoot string) error {
	if len(s.Skills) == 0 {
		return fmt.Errorf("%s: skills 为空", RegistrySnapshotPath)
	}
	seen := map[string]bool{}
	for _, sk := range s.Skills {
		if sk.Name == "" || seen[sk.Name] {
			return fmt.Errorf("%s: skill name 缺失或重复（%q）", RegistrySnapshotPath, sk.Name)
		}
		seen[sk.Name] = true
		if sk.Type != "service" {
			return fmt.Errorf("%s: skill %s type=%q，收口后的能力必须是 service（网关按 addr 路由）",
				RegistrySnapshotPath, sk.Name, sk.Type)
		}
		want := skillgateway.CutoverTargetForSkill(sk.Name)
		if want == "" {
			return fmt.Errorf("%s: skill %s 不在 MIG-1 收口范围（x-scan / like）", RegistrySnapshotPath, sk.Name)
		}
		if got := strings.TrimSuffix(strings.TrimSpace(sk.Addr), "/"); got != want {
			return fmt.Errorf("%s: skill %s 的 addr = %q，与网关口径 %q 不一致",
				RegistrySnapshotPath, sk.Name, got, want)
		}
		if sk.Version == "" || sk.Image == "" {
			return fmt.Errorf("%s: skill %s 缺 version/image（注册表要能唯一确定实现）",
				RegistrySnapshotPath, sk.Name)
		}
	}
	for _, lg := range s.Decommissioned {
		wantSvc := skillgateway.LegacyServiceForSkill(lg.Skill)
		if wantSvc == "" {
			return fmt.Errorf("%s: decommissioned 里的 skill %q 不在收口范围", RegistrySnapshotPath, lg.Skill)
		}
		if lg.Service != wantSvc {
			return fmt.Errorf("%s: skill %s 的旧服务 = %q，与网关口径 %q 不一致",
				RegistrySnapshotPath, lg.Skill, lg.Service, wantSvc)
		}
		if wantAddr := skillgateway.LegacyAddrForSkill(lg.Skill); lg.Addr != wantAddr {
			return fmt.Errorf("%s: skill %s 的旧 addr = %q，与网关口径 %q 不一致",
				RegistrySnapshotPath, lg.Skill, lg.Addr, wantAddr)
		}
		if lg.Compose == "" {
			return fmt.Errorf("%s: 旧服务 %s 缺 compose 路径（回滚时要把服务拉回来）", RegistrySnapshotPath, lg.Service)
		}
		if repoRoot != "" {
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(lg.Compose))); err != nil {
				return fmt.Errorf("%s: 旧服务 %s 的 compose %s 不存在（%v）",
					RegistrySnapshotPath, lg.Service, lg.Compose, err)
			}
		}
	}
	return nil
}

// IncludedComposeFiles 返回 deploy/docker-compose.yml 的 include 清单（相对仓库根，原序）。
func IncludedComposeFiles(repoRoot string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "docker-compose.yml"))
	if err != nil {
		return nil, fmt.Errorf("read deploy/docker-compose.yml: %w", err)
	}
	var root struct {
		Include []string `yaml:"include"`
	}
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse deploy/docker-compose.yml: %w", err)
	}
	out := make([]string, 0, len(root.Include))
	for _, rel := range root.Include {
		out = append(out, filepath.ToSlash(filepath.Join("deploy", strings.TrimSpace(rel))))
	}
	return out, nil
}

// DecommissionPlanFromSnapshot 把快照里的旧服务换成 migration 的下线判定入参
// （DecommissionService.Evaluate 的 services 参数），让「下线清单」与「注册表快照」同源。
func (s *RegistrySnapshot) DecommissionPlanFromSnapshot() []LegacyService {
	out := make([]LegacyService, 0, len(s.Decommissioned))
	for _, lg := range s.Decommissioned {
		out = append(out, LegacyService{Service: lg.Service, Skill: lg.Skill, Addr: lg.Addr})
	}
	return out
}

// RegistrySnapshotRoutes 挂载注册表快照的两个运维端点（调用方必须挂 requireAuth + admin，
// 与 /skill-cutover/* 同一口径 —— 同步会往注册表写版本行，是改路由池的动作）：
//
//	GET  /skill-registry/snapshot       装载 + 校验快照，逐条给出与网关口径的比对结果
//	POST /skill-registry/snapshot/sync  把快照登记进注册表（?dry_run=1 只预演不写）
//
// 为什么开成端点而不只留 CLI：board 运维中心的切流页要在**放量之前**回答「x-scan / like 的
// 新实现登记了没有、addr 对不对」，运维在同一个页面里跑完「同步 → 登记 target → 逐档放量」。
func RegistrySnapshotRoutes(r gin.IRouter, repoRoot string, gdb *gorm.DB, mw gin.HandlerFunc, mws ...gin.HandlerFunc) {
	if mw == nil {
		panic("migration: /skill-registry/* 的鉴权中间件不能为 nil（同步会往注册表写版本行）")
	}
	registerRegistryRoutes(r.Group("/skill-registry", append([]gin.HandlerFunc{mw}, mws...)...), repoRoot, gdb)
}

// registerRegistryRoutes 挂两个端点；gdb 可为 nil（只读端点照常工作，同步端点会明确报
// 「需要 MySQL」，不会静默成功）。
func registerRegistryRoutes(g gin.IRoutes, repoRoot string, gdb *gorm.DB) {
	g.GET("/snapshot", func(c *gin.Context) {
		snap, err := LoadRegistrySnapshot(repoRoot)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := snap.Validate(repoRoot); err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": false, "snapshot": snap, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "snapshot": snap})
	})
	g.POST("/snapshot/sync", func(c *gin.Context) {
		snap, err := LoadRegistrySnapshot(repoRoot)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := snap.Validate(repoRoot); err != nil {
			c.JSON(http.StatusConflict, gin.H{"ok": false, "error": err.Error()})
			return
		}
		dryRun := c.Query("dry_run") == "1"
		res, err := snap.SyncRegistry(c.Request.Context(), gdb, dryRun)
		if res == nil {
			// 装载/环境级失败（无库、读不到快照）：没有逐条结果可给，dry_run 也要如实回显，
			// 否则 board 上的预演按钮点完只显示一个 error，运维不知道刚才那次是不是预演。
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "dry_run": dryRun, "error": err.Error()})
			return
		}
		code := http.StatusOK
		if err != nil {
			code = http.StatusConflict // 拒绝项 = 可解释的业务冲突（board 要显示逐条原因）
		}
		body := gin.H{"ok": err == nil, "dry_run": res.DryRun, "changes": res.Changes}
		if err != nil {
			body["error"] = err.Error()
		}
		c.JSON(code, body)
	})
}
