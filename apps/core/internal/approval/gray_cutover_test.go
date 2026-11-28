// S1 灰度状态机与 M4-1 跨服务切流的隔离测试（工作项 wi-1795489658-2d59）。
//
// 两边共用 skill_versions.status='gray'，档位语义却完全不同：S1 是 5→50→100 的自动推进，
// M4-1 是 10→50→100 的人工逐档放量。TickGray（cmd/api 的 1 分钟 ticker）若把切流的 gray
// 版本当成自己的灰度版本接管，人工放量护栏（观察间隔 + 一致率/成功率门槛）会被整体绕过。
package approval

import (
	"context"
	"testing"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newGrayTestStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE skills (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, owner TEXT NOT NULL DEFAULT '',
		 runtime TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT '', entrypoint TEXT NOT NULL DEFAULT '', addr TEXT NOT NULL DEFAULT '',
		 container TEXT NOT NULL DEFAULT '', limits TEXT NOT NULL DEFAULT '{}', permissions TEXT NOT NULL DEFAULT '{}',
		 input_schema TEXT NOT NULL DEFAULT '{}', output_schema TEXT NOT NULL DEFAULT '{}',
		 created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE skill_versions (id INTEGER PRIMARY KEY AUTOINCREMENT, skill_id INTEGER NOT NULL, version TEXT NOT NULL,
		 image TEXT NOT NULL DEFAULT '', traffic_split INTEGER NOT NULL DEFAULT 100, status TEXT NOT NULL DEFAULT 'draft',
		 rollback_to TEXT NOT NULL DEFAULT '', config TEXT NOT NULL DEFAULT '{}', gray_stage INTEGER NULL,
		 gray_stage_started_at DATETIME NULL, approval_id INTEGER NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		 UNIQUE (skill_id, version))`,
		`CREATE TABLE skill_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, skill_name TEXT NOT NULL, version TEXT NOT NULL,
		 trace_id TEXT NOT NULL DEFAULT '', workflow_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '',
		 input_hash TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', latency_ms INTEGER NULL, error TEXT NULL,
		 created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE audit_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NULL, actor TEXT NOT NULL,
		 action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '', note TEXT, detail TEXT,
		 created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE evolve_tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, task_no TEXT NOT NULL DEFAULT '',
		 approval_id INTEGER NULL, status TEXT NOT NULL DEFAULT 'draft', created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		 updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE skill_cutover (id INTEGER PRIMARY KEY AUTOINCREMENT, skill_name TEXT NOT NULL UNIQUE,
		 target_addr TEXT NOT NULL DEFAULT '', target_version TEXT NOT NULL DEFAULT '', legacy_version TEXT NOT NULL DEFAULT '',
		 traffic INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'pending', stage_at DATETIME NULL,
		 completed_at DATETIME NULL, parity_min REAL NOT NULL DEFAULT 0, success_min REAL NOT NULL DEFAULT 0,
		 min_samples INTEGER NOT NULL DEFAULT 0, actor TEXT NOT NULL DEFAULT '', note TEXT,
		 created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if err := gdb.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return &Store{db: &db.DB{DB: gdb}, cfg: &Config{GrayStageWindowSec: 1800, GrayStageMinRuns: 200, GrayFailRate: 0.2}}, gdb
}

// seedCutoverGray 灌一个「M4-1 切流刚放过第一档」的版本行：gray + 10% + gray_stage=10，
// 窗口起点在 40 分钟前（远超 1800s 的窗口），也就是 TickGray 一到就会判定「窗口到期」。
// 同时写 skill_cutover 的切流登记（running / 10%）：归属判据读的就是这张表。
func seedCutoverGray(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	if err := gdb.Exec(`INSERT INTO skill_cutover (skill_name, target_addr, target_version, legacy_version, traffic, status)
		VALUES ('like', 'http://social:8010/like', 'v2', 'v1', 10, 'running')`).Error; err != nil {
		t.Fatalf("seed skill_cutover: %v", err)
	}
	if err := gdb.Exec(`INSERT INTO skills (id, name, addr) VALUES (1, 'like', 'http://pyreplay:9500/like')`).Error; err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	started := time.Now().Add(-40 * time.Minute)
	stage := 10
	v := db.SkillVersion{SkillID: 1, Version: "v2", Image: "registry.local/like:v1",
		TrafficSplit: 10, Status: "gray", GrayStage: &stage, GrayStageStartedAt: &started}
	if err := gdb.Create(&v).Error; err != nil {
		t.Fatalf("seed cutover version: %v", err)
	}
	live := db.SkillVersion{SkillID: 1, Version: "v1", Image: "registry.local/like:v1",
		TrafficSplit: 90, Status: "live"}
	if err := gdb.Create(&live).Error; err != nil {
		t.Fatalf("seed baseline version: %v", err)
	}
}

func versionRow(t *testing.T, gdb *gorm.DB, version string) db.SkillVersion {
	t.Helper()
	var v db.SkillVersion
	if err := gdb.Where("version = ?", version).First(&v).Error; err != nil {
		t.Fatalf("version %s: %v", version, err)
	}
	return v
}

// TestTickGraySkipsCutoverVersions 钉住：M4-1 切流的 gray 版本绝不进 S1 灰度状态机。
func TestTickGraySkipsCutoverVersions(t *testing.T) {
	ctx := context.Background()
	s, gdb := newGrayTestStore(t)
	seedCutoverGray(t, gdb)

	changed, err := s.TickGray(ctx)
	if err != nil {
		t.Fatalf("tick gray: %v", err)
	}
	if changed != 0 {
		t.Fatalf("切流的 gray 版本不许被 S1 灰度状态机接管（处理了 %d 个）", changed)
	}

	v2 := versionRow(t, gdb, "v2")
	if v2.Status != "gray" || v2.TrafficSplit != 10 {
		t.Fatalf("切流版本必须保持 gray/10%%（放量由人工护栏决定），得到 %s/%d%%", v2.Status, v2.TrafficSplit)
	}
	if v2.GrayStage == nil || *v2.GrayStage != 10 {
		t.Fatalf("切流档位不许被 S1 改写（5→50→100），得到 %v", v2.GrayStage)
	}
	v1 := versionRow(t, gdb, "v1")
	if v1.Status != "live" || v1.TrafficSplit != 90 {
		t.Fatalf("基线版本不许被 S1 灰度置 retired/清零（那会让线上 100%% 打到新实现），得到 %s/%d%%", v1.Status, v1.TrafficSplit)
	}
}

// TestTickGrayStillDrivesApprovalGray 反向对照：带 approval_id 的 S1 灰度版本必须照旧推进，
// 证明上面的隔离是「按归属跳过」，不是把整个状态机短路了。
func TestTickGrayStillDrivesApprovalGray(t *testing.T) {
	ctx := context.Background()
	s, gdb := newGrayTestStore(t)
	seedCutoverGray(t, gdb)

	started := time.Now().Add(-40 * time.Minute)
	stage := 5
	approvalID := uint(7)
	if err := gdb.Create(&db.SkillVersion{SkillID: 1, Version: "v9", Image: "registry.local/like:v9",
		TrafficSplit: 5, Status: "gray", GrayStage: &stage, GrayStageStartedAt: &started, ApprovalID: &approvalID}).Error; err != nil {
		t.Fatalf("seed s1 gray version: %v", err)
	}

	changed, err := s.TickGray(ctx)
	if err != nil {
		t.Fatalf("tick gray: %v", err)
	}
	if changed != 1 {
		t.Fatalf("S1 自己的灰度版本必须照旧被处理，得到 changed=%d", changed)
	}
	v9 := versionRow(t, gdb, "v9")
	if v9.GrayStage == nil || *v9.GrayStage != 50 {
		t.Fatalf("S1 灰度应推进到 50%%，得到 %v", v9.GrayStage)
	}
	// 同一次 tick 里的切流版本必须原样不动：证明隔离是「按归属逐行跳过」，
	// 不是「碰到切流行就退出整个 tick」（那会让 S1 灰度跟着停摆）。
	if v2 := versionRow(t, gdb, "v2"); v2.Status != "gray" || v2.TrafficSplit != 10 {
		t.Fatalf("同批里的切流版本必须不受影响，得到 %s/%d%%", v2.Status, v2.TrafficSplit)
	}
}

// TestTickGrayStillDrivesDeployctlGray 钉住「无审批单的普通灰度」绝不被冻结。
//
// deployctl version up <skill> <ver> --env gray（internal/deploy/version.go）写的行是
// status=gray + traffic_split=5 + approval_id IS NULL —— 与 M4-1 切流行的形状完全一样。
// 若归属判据用「approval_id IS NULL」，这类灰度会被永久冻结在 5%：不升档、恶化不回滚、不告警。
// 所以判据必须是「这一行确实是某个未收尾切流的目标版本」，按 skill+版本精确匹配。
func TestTickGrayStillDrivesDeployctlGray(t *testing.T) {
	ctx := context.Background()
	s, gdb := newGrayTestStore(t)
	seedCutoverGray(t, gdb) // like 有未收尾的切流（目标 v2）

	started := time.Now().Add(-40 * time.Minute)
	stage := 5
	// x-scan 上一个 deployctl 起的灰度：无审批单，且与任何切流登记无关。
	if err := gdb.Exec(`INSERT INTO skills (id, name, addr) VALUES (2, 'x-scan', 'http://pycraper:9400/x-scan')`).Error; err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	if err := gdb.Create(&db.SkillVersion{SkillID: 2, Version: "v7", Image: "registry.local/x-scan:v7",
		TrafficSplit: 5, Status: "gray", GrayStage: &stage, GrayStageStartedAt: &started}).Error; err != nil {
		t.Fatalf("seed deployctl gray: %v", err)
	}

	changed, err := s.TickGray(ctx)
	if err != nil {
		t.Fatalf("tick gray: %v", err)
	}
	if changed != 1 {
		t.Fatalf("deployctl 起的灰度必须照旧被推进（changed=%d）", changed)
	}
	var v7 db.SkillVersion
	if err := gdb.Where("version = ?", "v7").First(&v7).Error; err != nil {
		t.Fatalf("v7: %v", err)
	}
	if v7.GrayStage == nil || *v7.GrayStage != 50 {
		t.Fatalf("deployctl 灰度应推进到 50%%，得到 %v", v7.GrayStage)
	}
	// 同批的切流版本必须让路。
	var v2 db.SkillVersion
	if err := gdb.Where("version = ?", "v2").First(&v2).Error; err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.Status != "gray" || v2.TrafficSplit != 10 {
		t.Fatalf("切流目标版本必须保持 gray/10%%，得到 %s/%d%%", v2.Status, v2.TrafficSplit)
	}
}

// TestTickGrayReclaimsCompletedCutover 钉住「切流收尾后 S1 重新接管」：
// 收尾之后切流状态机不再动这些行，若继续让路，一个 100% 的 gray 行会永远没人推进/回滚。
// TestTickGrayReclaimsRolledBackCutover 钉住「切流回滚后 S1 重新接管」（评审 bug：守卫曾写
// `status <> 'completed'`，把 rolled_back 也算成「未收尾」）。
//
// Rollback 只把切流行改成 rolled_back，不清 target_version；回滚时目标版本行被置 retired/0，
// 所以「切流回滚 → 同一版本号再走普通灰度（cd.startGrayRelease 复用已有 (skill_id, version)
// 行、deployctl version up --env gray 同理）」会留下一条永久命中的 {skill_name, target_version}
// 键。按旧写法这条 gray 行被永久冻结：不升档、恶化不回滚、也不告警。
func TestTickGrayReclaimsRolledBackCutover(t *testing.T) {
	ctx := context.Background()
	s, gdb := newGrayTestStore(t)
	seedCutoverGray(t, gdb)
	// 切流回滚：切流行 rolled_back（target_version 原样留着），目标版本行 retired/0。
	if err := gdb.Exec(`UPDATE skill_cutover SET status = 'rolled_back', traffic = 0 WHERE skill_name = 'like'`).Error; err != nil {
		t.Fatalf("rollback cutover: %v", err)
	}
	if err := gdb.Exec(`UPDATE skill_versions SET status = 'retired', traffic_split = 0, gray_stage = NULL WHERE version = 'v2'`).Error; err != nil {
		t.Fatalf("retire cutover version: %v", err)
	}
	// 同一版本号再走普通灰度（复用那一行，档位回到 5%、窗口已到期）。
	if err := gdb.Exec(`UPDATE skill_versions SET status = 'gray', traffic_split = 5, gray_stage = 5 WHERE version = 'v2'`).Error; err != nil {
		t.Fatalf("restart gray on v2: %v", err)
	}

	changed, err := s.TickGray(ctx)
	if err != nil {
		t.Fatalf("tick gray: %v", err)
	}
	if changed != 1 {
		t.Fatalf("已回滚的切流版本必须回到 S1 灰度状态机（changed=%d）：陈旧 rolled_back 记录把 target_version 留着，"+
			"按「未收尾」口径会让这条 gray 行永久冻结", changed)
	}
}

func TestTickGrayReclaimsCompletedCutover(t *testing.T) {
	ctx := context.Background()
	s, gdb := newGrayTestStore(t)
	seedCutoverGray(t, gdb)
	if err := gdb.Exec(`UPDATE skill_cutover SET status = 'completed', traffic = 100 WHERE skill_name = 'like'`).Error; err != nil {
		t.Fatalf("complete cutover: %v", err)
	}

	changed, err := s.TickGray(ctx)
	if err != nil {
		t.Fatalf("tick gray: %v", err)
	}
	if changed != 1 {
		t.Fatalf("已收尾的切流版本必须回到 S1 灰度状态机（changed=%d）", changed)
	}
}
