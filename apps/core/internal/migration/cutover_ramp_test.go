// M4-1 切流状态机的端到端单测：10% → 50% → 100% 逐档放量 + 回滚 + 收尾 + 下线清单。
//
// 与 cutover_test.go（纯函数层）配套，本文件跑完整链路：登记目标 → 灌证据 → 升档 →
// 收尾（skills.addr 收敛）→ 旧服务下线判定。护栏不过时**必须**是「带原因的拒绝」，
// 因为 board 上运维要点开看到「差在哪」，一个笼统的 500 等于让人盲操作。
//
// 基础设施口径与本仓库既有测试一致（apps/core/internal/approval/*_test.go）：
// sqlite :memory: + 显式建表，CI 不需要 MySQL / Redis / Qdrant / 浏览器。
package migration

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
)

const (
	likeTarget   = "http://social:8010/like"
	xScanTarget  = "http://social:8010/x-scan"
	legacyLike   = "http://pyreplay:9500/like"
	legacyXScan  = "http://pycraper:9504/scan"
	baselineVer  = "v1"
	cutoverVer   = "v2"
	cutoverGroup = "parity-batch-2026-12-05/0001"
)

type cutFixture struct {
	db       *gorm.DB
	svc      *CutoverService
	logs     []logEntry
	alerts   []notifyAlert
	now      time.Time
	probeSeq int
}

// newCutFixture 建一套最小 skill 网关数据：like / x-scan 各一个 live 基线版本（旧实现 addr）。
func newCutFixture(t *testing.T) *cutFixture {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: glogger.Default.LogMode(glogger.Silent)})
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
	} {
		if err := gdb.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}

	f := &cutFixture{db: gdb, now: time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)}
	f.svc = NewCutover(gdb, CutoverConfig{Actor: "ops", MinStageInterval: 30 * time.Minute, MinSamples: 20, ParityMin: 0.9, SuccessMin: 0.95},
		func(_ context.Context, title, body, level string) error {
			f.alerts = append(f.alerts, notifyAlert{title, body, level})
			return nil
		})
	f.svc.now = func() time.Time { return f.now }
	f.svc.SetProbePrefix(cutoverGroup) // 探针组名 = `<前缀>-<批次号>`（批次号在尾部）
	// sqlite 不支持生产 SQL 里的 `skill_id = (SELECT ...)` 关联子查询写法，这里按等价语义注入。
	f.svc.versionLookup = func(_ context.Context, skillName, version string) (*db.SkillVersion, error) {
		var v db.SkillVersion
		err := gdb.Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND version = ?", skillName, version).First(&v).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
	// 收尾后的回滚要恢复 legacy_version 那一行，而它已被 Complete 写成 retired/0：
	// lookupAnyVersion 必须能按名字取到任意 status 的版本行（sqlite 不支持生产 SQL 的关联子查询，
	// 这里按等价语义注入；生产路径 = lookupAnyVersion 的 `skill_id = (SELECT ...)`）。
	f.svc.anyVersionLookup = func(_ context.Context, skillName, version string) (*db.SkillVersion, error) {
		var v db.SkillVersion
		err := gdb.Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND version = ?", skillName, version).First(&v).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
	f.svc.legacyLookup = func(_ context.Context, skillName, excludeVersion string) (*db.SkillVersion, error) {
		var v db.SkillVersion
		err := gdb.Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND version <> ?", skillName, excludeVersion).
			Order("skill_versions.id ASC").First(&v).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
	f.svc.auditFn = func(_ context.Context, actor, action, target string, detail any) error {
		d, _ := detail.(map[string]any)
		f.logs = append(f.logs, logEntry{actor, action, target, d})
		return nil
	}
	if err := f.svc.EnsureSchema(); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	for _, s := range []struct{ name, addr string }{{"like", legacyLike}, {"x-scan", legacyXScan}} {
		if err := gdb.Create(&db.Skill{Name: s.name, Owner: "ops", Runtime: "python", Type: "service", Addr: s.addr}).Error; err != nil {
			t.Fatalf("create skill %s: %v", s.name, err)
		}
		cfg, err := json.Marshal(map[string]any{"addr": s.addr})
		if err != nil {
			t.Fatalf("marshal cfg: %v", err)
		}
		if err := gdb.Create(&db.SkillVersion{SkillID: f.skillID(t, s.name), Version: baselineVer,
			Image: "registry.local/" + s.name + ":v1", TrafficSplit: 100, Status: "live", Config: string(cfg), CreatedAt: f.now}).Error; err != nil {
			t.Fatalf("create baseline version %s: %v", s.name, err)
		}
	}
	return f
}

// svcRow 读回切流行（断言 stage_at / traffic 等被服务写进库的值）。
func (f *cutFixture) svcRow(t *testing.T, ctx context.Context) *db.SkillCutover {
	t.Helper()
	row, err := f.svc.Get(ctx, "like")
	if err != nil || row == nil {
		t.Fatalf("get skill_cutover: %v", err)
	}
	return row
}

func (f *cutFixture) skillID(t *testing.T, name string) uint {
	t.Helper()
	var s db.Skill
	if err := f.db.Where("name = ?", name).First(&s).Error; err != nil {
		t.Fatalf("skill %s: %v", name, err)
	}
	return s.ID
}

func (f *cutFixture) addrOf(t *testing.T, name string) string {
	t.Helper()
	var s db.Skill
	if err := f.db.Where("name = ?", name).First(&s).Error; err != nil {
		t.Fatalf("skill %s: %v", name, err)
	}
	return s.Addr
}

func (f *cutFixture) traffic(t *testing.T, name, version string) (int, string) {
	t.Helper()
	var v db.SkillVersion
	if err := f.db.Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND version = ?", name, version).First(&v).Error; err != nil {
		t.Fatalf("version %s/%s: %v", name, version, err)
	}
	return v.TrafficSplit, v.Status
}

// trafficRaw 绕开 GORM 直接读库里的 traffic_split。
//
// 为什么需要它：sqlite 驱动在 UPDATE 时会把**主键定位**的列一起带上（`SET ..., id=?, skill_id=?, ...`），
// 于是「内存对象里是 0、UPDATE 没点名 traffic_split」这类缺陷在 sqlite 上会被顺带写对，
// 只有 MySQL（UPDATE 只下发被显式点名的列）才暴露。断言「流量真的归零」必须读裸列，
// 不能被驱动的这层行为掩盖。
func (f *cutFixture) trafficRaw(t *testing.T, name, version string) int {
	t.Helper()
	var n int
	if err := f.db.Raw("SELECT traffic_split FROM skill_versions WHERE skill_id = (SELECT id FROM skills WHERE name = ?) AND version = ?",
		name, version).Scan(&n).Error; err != nil {
		t.Fatalf("raw traffic %s/%s: %v", name, version, err)
	}
	return n
}

// fillParity 灌离线双跑探针：groups 个组，每组新旧实现各记一条；前 parityBad 组的
// 新实现结果与旧实现不同（= 该组不一致）。
//
// 组名用全局自增序号：同一 skill 的多批探针必须落进不同组，否则「补跑一批全一致的组」
// 会按组名覆盖掉此前不一致的样本，一致率被算成假的全绿。
func (f *cutFixture) fillParity(t *testing.T, skill string, groups, parityBad int) {
	t.Helper()
	ctx := context.Background()
	// 一批探针 = 一个批次号（越大越新）：组标识 = `<前缀>-<4 位批次号>-<组内序号>`，组内
	// 序号让同批各组互不覆盖，批次号让新批次的结论压得过旧批次（首档门槛只看最新一批）。
	f.probeSeq++
	for i := 0; i < groups; i++ {
		group := ParityProbeGroup(cutoverGroup, f.probeSeq, i+1)
		badOld := i < parityBad
		// 旧实现（基线版本）先记，新实现后记；不一致 = 新实现摘要不同。
		if err := f.svc.RecordParityRun(ctx, skill, baselineVer, group, hash64("in-"+group), `{"count":3}`, true); err != nil {
			t.Fatalf("record legacy probe: %v", err)
		}
		sum := `{"count":3}`
		if badOld {
			sum = `{"count":4}`
		}
		if err := f.svc.RecordParityRun(ctx, skill, cutoverVer, group, hash64("in-"+group), sum, true); err != nil {
			t.Fatalf("record target probe: %v", err)
		}
	}
}

// fillRuns 灌生产调用：n 条（其中 successBad 条失败）。
func (f *cutFixture) fillRuns(t *testing.T, skill string, n, successBad int) {
	t.Helper()
	for i := 0; i < n; i++ {
		status := "ok"
		if i < successBad {
			status = "fail"
		}
		if err := f.db.Create(&db.SkillRun{SkillName: skill, Version: cutoverVer, TraceID: hash64("trace-" + cutoverVer)[:32],
			InputHash: hash64("prod-" + cutoverVer), Status: status, CreatedAt: f.now}).Error; err != nil {
			t.Fatalf("insert prod run: %v", err)
		}
	}
}

// fillRunsVer 同 fillRuns，但把生产调用记在指定版本名下（旧实现仍在接流量时用）。
func (f *cutFixture) fillRunsVer(t *testing.T, skill, version string, n, successBad int) {
	t.Helper()
	for i := 0; i < n; i++ {
		status := "ok"
		if i < successBad {
			status = "fail"
		}
		if err := f.db.Create(&db.SkillRun{SkillName: skill, Version: version,
			TraceID: hash64("trace-" + version)[:32], InputHash: hash64("prod-" + version),
			Status: status, CreatedAt: f.now}).Error; err != nil {
			t.Fatalf("insert legacy run: %v", err)
		}
	}
}

// fill 一次性灌满「升档达标」所需的证据：groups 组离线双跑 + runs 条生产调用。
func (f *cutFixture) fill(t *testing.T, skill string, groups, parityBad, runs, successBad int) {
	t.Helper()
	f.fillParity(t, skill, groups, parityBad)
	f.fillRuns(t, skill, runs, successBad)
}

func hash64(s string) string {
	return sha256hex([]byte(s))
}

func (f *cutFixture) hasLog(action string) bool {
	for _, l := range f.logs {
		if l.action == action {
			return true
		}
	}
	return false
}

func (f *cutFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func TestCutoverFullRamp10To50To100(t *testing.T) {
	f := newCutFixture(t)
	ctx := context.Background()

	// 未登记目标就放量 → 拒绝（护栏在写之前）。
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); !errors.Is(err, ErrCutoverNotParsed) {
		t.Fatalf("未登记目标应拒绝放量，得到 %v", err)
	}
	if n, _ := f.traffic(t, "like", baselineVer); n != 100 {
		t.Fatalf("被拒绝的放量不该动流量，基线档位 = %d", n)
	}

	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", "M4-1 收口到 social"); err != nil {
		t.Fatalf("target: %v", err)
	}
	// 登记目标不改 skills.addr：它是基线服务地址（S5 版本容器 / deployctl 靠它找回服务），
	// 灰度期改它会把「没被分流命中」的调用一起打到新实现。
	if got := f.addrOf(t, "like"); got != legacyLike {
		t.Fatalf("登记目标后 skills.addr 必须不变，得到 %q", got)
	}

	// 第一档 10%：门槛是「离线双跑全部一致」，没跑双跑就切不上去（零证据不放量）。
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err == nil ||
		!strings.Contains(err.Error(), "离线双跑") {
		t.Fatalf("没有对比探针时禁止上灰度，得到 %v", err)
	}
	f.fillParity(t, "like", 25, 0) // 首档要求双跑全绿；升档要求样本量 ≥20
	v10, ev, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops")
	if err != nil {
		t.Fatalf("stage 10: %v", err)
	}
	if v10.TrafficSplit != 10 {
		t.Fatalf("10%% 档写入异常: %d", v10.TrafficSplit)
	}
	if n, st := f.traffic(t, "like", baselineVer); n != 90 || st != "live" {
		t.Fatalf("10%% 档旧实现应承接 90%%，得到 %d/%s", n, st)
	}
	if !strings.Contains(v10.Config, likeTarget) {
		t.Fatalf("切流版本的 config.addr 必须指向 social，得到 %q", v10.Config)
	}
	if ev.Satisfied(f.svc.Config()) {
		t.Fatal("刚起步零证据不该判达标")
	}

	// 观察时长不足 → 拒绝升 50%（即便生产数据全绿）。
	f.fillRuns(t, "like", 30, 0)
	if _, err := f.svc.Get(ctx, "like"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 50, "ops"); !errors.Is(err, ErrCutoverInterval) {
		t.Fatalf("观察时长不足应拒绝升档，得到 %v", err)
	}

	// 时长到 + 数据达标 → 50%（生产样本窗口起点 = 上一档 stage_at，必须在放量之后写入）。
	f.advance(31 * time.Minute)
	if v50, ev50, err := f.svc.Stage(ctx, "like", cutoverVer, 50, "ops"); err != nil {
		t.Fatalf("stage 50: %v (%+v)", err, ev50)
	} else if v50.TrafficSplit != 50 {
		t.Fatalf("50%% 档写入异常: %d", v50.TrafficSplit)
	}
	if n, _ := f.traffic(t, "like", baselineVer); n != 50 {
		t.Fatalf("50%% 档旧实现应承接 50%%，得到 %d", n)
	}

	// 100% 档同样要过质量护栏：本档窗口（50% 档起点之后）里必须有足量成功样本。
	f.fillRuns(t, "like", 25, 0)
	f.advance(31 * time.Minute)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 100, "ops"); err != nil {
		t.Fatalf("stage 100: %v", err)
	}
	if n, _ := f.traffic(t, "like", baselineVer); n != 0 {
		t.Fatalf("100%% 档旧实现流量应归零，得到 %d", n)
	}

	// 收尾：100% 档观察窗内同样要有足量成功样本；达标后 skills.addr 收敛到 social。
	f.fillRuns(t, "like", 25, 0)
	if _, _, err := f.svc.Complete(ctx, "like", "ops"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := f.addrOf(t, "like"); got != likeTarget {
		t.Fatalf("全量收尾后 skills.addr 应指向 social，得到 %q", got)
	}
	row, err := f.svc.Get(ctx, "like")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != CutoverCompleted || row.Traffic != 100 || row.LegacyVer != baselineVer {
		t.Fatalf("收尾后切流行异常: %+v", row)
	}
	for _, action := range []string{"cutover_target", "cutover_stage", "cutover_complete"} {
		if !f.hasLog(action) {
			t.Fatalf("缺少审计动作 %q（切流全程必须留痕）", action)
		}
	}

	// 收尾幂等：Complete 会把 skills.addr 收敛到目标 addr 并 retire 其余版本，重复执行等于
	// 让「基线服务」一路漂移，回滚与 S5 版本容器都会找错服务。必须在收尾当刻判（回滚之后
	// status 已变 rolled_back，那时再测测的是另一条护栏）。
	if _, _, err := f.svc.Complete(ctx, "like", "ops"); !errors.Is(err, ErrCutoverCompleted) {
		t.Fatalf("重复收尾必须被拦下，得到 %v", err)
	}

	// 已全量后禁止换目标（只能回滚重放）。
	if _, err := f.svc.Target(ctx, "like", "http://other:1/x", "ops", ""); err == nil {
		t.Fatal("已全量后禁止更换目标")
	}

	// 回滚：新实现归零并 retired，旧实现恢复 100%，切流行 rolled_back，并告警 + 留痕。
	if err := f.svc.Rollback(ctx, "like", "一致率跌破阈值", "ops"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 0 || st != "retired" {
		t.Fatalf("回滚后切流版本应 0%%/retired，得到 %d/%s", n, st)
	}
	if n, st := f.traffic(t, "like", baselineVer); n != 100 || st != "live" {
		t.Fatalf("回滚后旧实现应恢复 100%%/live，得到 %d/%s", n, st)
	}
	row, _ = f.svc.Get(ctx, "like")
	if row.Status != CutoverRolledBack || row.Traffic != 0 {
		t.Fatalf("回滚后切流行异常: %+v", row)
	}
	if !f.hasLog("cutover_rollback") || len(f.alerts) == 0 {
		t.Fatal("回滚必须留痕 + 告警")
	}

	// 回滚之后禁止收尾（同理：对着已回滚的切流说「先去放量」会把人引回刚出问题实现）。
	if _, _, err := f.svc.Complete(ctx, "like", "ops"); !errors.Is(err, ErrCutoverNoTarget) {
		t.Fatalf("回滚后收尾必须被拦下，得到 %v", err)
	}

	// 回滚必须留下时间护栏：刚回滚完立刻重放，流量会打到同一份还没修好的实现上。
	// 「再等一个观察窗口」才允许重新上第一档（与升档同口径，间隔读 stage_at）。
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); !errors.Is(err, ErrCutoverInterval) {
		t.Fatalf("回滚后立刻重放必须被时长护栏拦下，得到 %v", err)
	}
	f.advance(31 * time.Minute)
	// 注意这里刻意**不灌任何生产样本**：回滚已把目标版本置 retired/traffic_split=0，
	// 之后不可能再产生该版本的生产调用。重放的门槛只能是「离线双跑」（见下一段）。
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("观察满窗口后应可重新上第一档（首档门槛 = 离线双跑，不是生产样本量）: %v", err)
	}
}

// TestCutoverRejectsVersionHijack 钉住「切流不许改写别人的版本行」：
// stage 的 version 填成现存基线（live/100%）版本号时，必须在任何写库动作之前被拒。
//
// 为什么这条必须单独钉：网关按**版本行 config 里的 addr** 路由，而 ensureTargetVersion 会
// 把 config.addr 改成切流目标。放行 = 流量在任何质量护栏执行之前就被切到新实现，且护栏
// 随后拒绝也救不回来（config 已落库，审计里只有一条被拒的调用）。
func TestCutoverRejectsVersionHijack(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	f.fillParity(t, "like", 6, 0) // 证据齐全：证明拦下它的不是质量护栏

	if _, _, err := f.svc.Stage(ctx, "like", baselineVer, 10, "ops"); !errors.Is(err, ErrCutoverVersionLocked) {
		t.Fatalf("拿基线版本号做切流必须被拒，得到 %v", err)
	}
	// 基线版本行必须一个字节都没被动过（addr 仍是旧实现、仍是 live/100%）。
	var v db.SkillVersion
	if err := f.db.Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND version = ?", "like", baselineVer).
		First(&v).Error; err != nil {
		t.Fatalf("查基线版本: %v", err)
	}
	if v.Status != "live" || v.TrafficSplit != 100 || !strings.Contains(v.Config, legacyLike) {
		t.Fatalf("被拒的切流绝不能改动基线版本行: %+v", v)
	}
	if !strings.Contains(f.addrOf(t, "like"), legacyLike) {
		t.Fatalf("skills.addr 也不该动: %s", f.addrOf(t, "like"))
	}
}

// TestCutoverNewVersionStartsAtZeroTraffic 钉住「版本存在」与「版本带流量」彻底解耦。
//
// 为什么必须单独钉：SkillVersion.TrafficSplit 带 gorm `default:100`，GORM 对带 default 标签
// 的零值字段不下发该列 → 落库取 DB 默认 100（内存里仍是 0，单测若只看内存对象完全看不出）。
// 新行 Status=gray 且 config.addr 已指向切流目标，网关 ResolveLive 按
// 「status IN (live,gray) AND traffic_split > 0」加权路由：不归零就等于「护栏还没跑，新实现
// 已经按 100% 参与路由」，首档被拒后还会以 gray+100% 永久留在库里，与基线各占一半流量。
func TestCutoverNewVersionStartsAtZeroTraffic(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}

	// 故意不灌探针：首档护栏必拒。拒绝之后新登记的版本行必须留在「零流量」状态。
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err == nil {
		t.Fatal("无探针必须拒绝首档")
	}
	// 护栏拒绝时**不许**把 target_version 落成空/落成未登记的版本号：approval.cutoverTargetVersions
	// 的让路判据读的就是这一列，脏值会让该版本被 S1 灰度状态机接管（详见 cutoverTargetVersions 注释）。
	if row, err := f.svc.Get(ctx, "like"); err != nil || row == nil || row.TargetVer != "" {
		t.Fatalf("首档被护栏拒绝时不许写 target_version: %+v (%v)", row, err)
	}
	var v db.SkillVersion
	if err := f.db.Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND version = ?", "like", cutoverVer).
		First(&v).Error; err != nil {
		t.Fatalf("查切流版本（护栏拒绝时版本行允许已建，但必须零流量）: %v", err)
	}
	// 读裸列：GORM 建表把该列建成 `NOT NULL DEFAULT 100`，GORM 的 Create 又跳过带 default
	// 标签的零值字段 → 库里是 100。用模型对象断言会被驱动/连接的读取行为掩盖，必须读裸列。
	if n := f.trafficRaw(t, "like", cutoverVer); n != 0 {
		t.Fatalf("被护栏拒绝的切流版本在库里必须 traffic_split=0，得到 %d（status=%s）", n, v.Status)
	}

	// 首档放行后也必须按档位写流量（10，不是 100）。
	f.fillParity(t, "like", 6, 0)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("stage 10: %v", err)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 10 || st != "gray" {
		t.Fatalf("首档应恰好 10%%/gray，得到 %d/%s", n, st)
	}
	// 基线承接剩余 90%，两者合计恒为 100。
	if n, _ := f.traffic(t, "like", baselineVer); n != 90 {
		t.Fatalf("基线应承接 90%%，得到 %d", n)
	}
}

// TestCutoverRollbackAbortsWhenNoSuccessor 钉住「找不到可接替版本时绝不留空路由池」
// （评审 bug：Rollback 先把切流版本 demote 成 retired/0，之后才找可恢复的旧版本；
// legacy_version 查不到且兜底 lookupLegacyVersion 也返回 nil 时，old == nil 分支什么都不恢复）。
//
// 后果是线上直接不可服务：库里再没有 `status IN (live,gray) AND traffic_split > 0` 的行 →
// skillgateway.Registry.ResolveLive 返回 ErrNoLiveVersion，该 skill 的调用全挂；而切流行已被置成
// rolled_back（Complete 被禁、Stage 首档要重过观察窗），状态机不会自愈，只能人工救。
func TestCutoverRollbackAbortsWhenNoSuccessor(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	skillID := f.skillID(t, "like")

	// 切流一路推到收尾：v2 = live/100，基线 v1 retired/0，skills.addr 已收敛到新实现。
	f.fillParity(t, "like", 25, 0)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("stage 10: %v", err)
	}
	f.fillRuns(t, "like", 25, 0)
	f.advance(31 * time.Minute)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 100, "ops"); err != nil {
		t.Fatalf("stage 100: %v", err)
	}
	f.fillRuns(t, "like", 25, 0)
	if _, _, err := f.svc.Complete(ctx, "like", "ops"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 100 || st != "live" {
		t.Fatalf("前置条件：收尾后切流版本必须 live/100%%，得到 %d/%s", n, st)
	}

	// 旧实现的版本行已被清理（收尾后下线旧服务的常见形态）：精确查与兜底查都查不到接替版本。
	if err := f.db.Exec("DELETE FROM skill_versions WHERE skill_id = ? AND version = ?", skillID, baselineVer).Error; err != nil {
		t.Fatalf("清理基线版本行: %v", err)
	}

	err := f.svc.Rollback(ctx, "like", "旧实现版本行已被清理时回滚", "ops")
	if err == nil {
		t.Fatal("没有可接替版本时回滚必须失败（不许静默留下空路由池）")
	}

	// 关键断言：路由池必须非空 —— 切流版本恢复回原档位/原 status（demote 已撤销）。
	var live int64
	if err := f.db.Raw(
		"SELECT COUNT(*) FROM skill_versions WHERE skill_id = ? AND status IN ? AND traffic_split > 0",
		skillID, []string{"live", "gray"}).Scan(&live).Error; err != nil {
		t.Fatalf("数路由池: %v", err)
	}
	if live == 0 {
		t.Fatalf("回滚失败后路由池不能为空（ResolveLive 会直接 ErrNoLiveVersion，该 skill 线上不可服务）")
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 100 || st != "live" {
		t.Fatalf("中止的回滚必须把切流版本原样恢复（demote 全部撤销），得到 %d/%s", n, st)
	}
	// 切流行也必须停在收尾状态：本次回滚整体没做成，运维补好旧版本行后可原样重试。
	row, err := f.svc.Get(ctx, "like")
	if err != nil || row == nil || row.Status != CutoverCompleted {
		t.Fatalf("中止的回滚不许把切流行改成 rolled_back: %+v (%v)", row, err)
	}
	// 基线 addr 同样不许回退（无 old 可参考，回写成不存在的地址会让 S5 起空容器）。
	if a := f.addrOf(t, "like"); a != likeTarget {
		t.Fatalf("中止的回滚不许改 skills.addr，得到 %s", a)
	}
}

// TestCutoverStageRejectsEmptyVersion 钉住「首档必须显式给版本号」（评审 bug：ensureTargetVersion
// 在 version 与 row.TargetVer 都为空时往下走「新建版本行」分支）。
//
// 空版本号建出来的 skill_versions 行 version=”，并把 skill_cutover.target_version 落成 ”。
// 后果不是脏数据而是护栏失效：approval.cutoverTargetVersions 用
// `target_version <> ” AND status IN (pending,running)` 判「这一行是不是切流版本」，空值让它
// 认不出切流行 → 该 gray 行被当成普通 S1 灰度接管 → advanceGray 对 gray_stage=10 落到 default
// （语义 = 100% 窗口健康 → 完成）→ 直接置 live/100 并 retire 基线，正是本包反复警告的
// 「绕过人工护栏全量」。routes.setStage 的 version 不是必填，HTTP 层可达。
func TestCutoverStageRejectsEmptyVersion(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}

	// 空 version + 首档档位：必须拒，且绝不许留下 version='' 的版本行。
	if _, _, err := f.svc.Stage(ctx, "like", "   ", 10, "ops"); err == nil {
		t.Fatal("空 version 必须拒绝（否则会建出 version 为空的版本行）")
	}
	var emptyRows int64
	if err := f.db.Table("skill_versions").Where("version = ?", "").Count(&emptyRows).Error; err != nil {
		t.Fatalf("数空版本行: %v", err)
	}
	if emptyRows != 0 {
		t.Fatalf("不许建出 version='' 的 skill_versions 行（现存 %d 条）", emptyRows)
	}
	// 跨档请求（50/100）同样不许建空版本行：档位护栏与空版本号护栏都要在写库之前拦住。
	for _, traffic := range []int{50, 100} {
		if _, _, err := f.svc.Stage(ctx, "like", "", traffic, "ops"); err == nil {
			t.Fatalf("空 version + %d%% 必须拒绝", traffic)
		}
	}
	emptyRows = 0
	if err := f.db.Table("skill_versions").Where("version = ?", "").Count(&emptyRows).Error; err != nil {
		t.Fatalf("数空版本行: %v", err)
	}
	if emptyRows != 0 {
		t.Fatalf("跨档请求也不许建出 version='' 的版本行（现存 %d 条）", emptyRows)
	}
	row, err := f.svc.Get(ctx, "like")
	if err != nil || row == nil || row.TargetVer != "" {
		t.Fatalf("被拒的 stage 不许把 target_version 落成空/改掉: %+v (%v)", row, err)
	}
	// 基线必须原样在线：被拒调用不得动任何流量。
	if n, st := f.traffic(t, "like", baselineVer); n != 100 || st != "live" {
		t.Fatalf("基线必须原样在线（%d/%s）", n, st)
	}
}

// TestCutoverFirstStageExemptionOnlyForFirstStage 钉住「首档豁免只对第一档生效」
// （评审 bug：isNew 判的是「该版本当前零流量」，不是「本次请求的是首档」）。
//
// 不钉住档位时，一次 POST /skill-cutover/:skill/stage 传 traffic_split=100 就能让新版本走首档
// 分支：只看离线双跑（FirstStageReasons），MinSamples / SuccessMin 全部跳过，且新建版本时
// row.StageAt 为 nil、时长护栏也不参与 —— 一次调用即可把生产流量 100% 打到从未跑过真实流量的
// 新实现。Complete 之后的 GateReasons 只拦「收尾」，那时流量已经全量。
func TestCutoverFirstStageExemptionOnlyForFirstStage(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	// 离线双跑「全绿」：证明拒绝绝不是因为探针不达标，而是档位本身被拒。
	f.fillParity(t, "like", 6, 0)

	for _, traffic := range []int{50, 100} {
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, traffic, "ops"); err == nil {
			t.Fatalf("新版本（零流量）请求 %d%% 必须被拒：首档豁免只对第一档 %d%% 生效，"+
				"否则一次调用即可绕过 MinSamples/SuccessMin 全量放量", traffic, CutoverStages[0])
		}
		if n := f.trafficRaw(t, "like", cutoverVer); n != 0 {
			t.Fatalf("被拒的 %d%% 请求不许写任何流量，得到 traffic_split=%d", traffic, n)
		}
	}
	// 第一档照常放行（豁免本身没被改坏）。
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, CutoverStages[0], "ops"); err != nil {
		t.Fatalf("首档 %d%% 必须放行: %v", CutoverStages[0], err)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != CutoverStages[0] || st != "gray" {
		t.Fatalf("首档应恰好 %d%%/gray，得到 %d/%s", CutoverStages[0], n, st)
	}
}

// TestCutoverRollbackWithoutStagedVersion 钉住「只登记了 target、从未放量」时的回滚：
// 目标版本行不存在（lookupVersionOptional 返回 (nil, nil)），回滚必须正常走完而不是 panic。
func TestCutoverRollbackWithoutStagedVersion(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	if err := f.svc.Rollback(ctx, "like", "上线前撤掉登记", "ops"); err != nil {
		t.Fatalf("未放量就回滚不该报错: %v", err)
	}
	row, err := f.svc.Get(ctx, "like")
	if err != nil || row == nil || row.Status != CutoverRolledBack {
		t.Fatalf("回滚后切流行异常: %+v %v", row, err)
	}
	if n, st := f.traffic(t, "like", baselineVer); n != 100 || st != "live" {
		t.Fatalf("基线必须原样在线（%d/%s）", n, st)
	}
}

func TestCutoverQualityGatesBlockRamp(t *testing.T) {
	ctx := context.Background()

	t.Run("样本不足不放量", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		f.fillParity(t, "like", 25, 0) // 首档门槛：离线双跑全部一致
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage 10: %v", err)
		}
		f.advance(31 * time.Minute)
		f.fillRuns(t, "like", 5, 0) // 阈值 20，生产样本只给 5 条
		_, ev, err := f.svc.Stage(ctx, "like", cutoverVer, 50, "ops")
		if err == nil {
			t.Fatal("样本不足必须拒绝放量")
		}
		if !strings.Contains(err.Error(), "样本不足") || len(ev.GateReasons(f.svc.Config())) == 0 {
			t.Fatalf("拒绝放量必须给出逐条原因，得到 %v / %+v", err, ev.GateReasons(f.svc.Config()))
		}
		if n, _ := f.traffic(t, "like", cutoverVer); n != 10 {
			t.Fatalf("被拒绝的放量不该改流量，得到 %d", n)
		}
	})

	t.Run("成功率不达标不放量", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		f.fillParity(t, "like", 30, 0)
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage 10: %v", err)
		}
		f.advance(31 * time.Minute)
		f.fillRuns(t, "like", 60, 5) // 60 条里 5 条失败 = 0.917 < 0.95
		_, _, err := f.svc.Stage(ctx, "like", cutoverVer, 50, "ops")
		if err == nil || !strings.Contains(err.Error(), "成功率") {
			t.Fatalf("成功率不达标必须点名成功率，得到 %v", err)
		}
	})

	t.Run("一致率不达标不放量", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "x-scan", xScanTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		// 首档门槛是「离线双跑全部一致」：双跑里只要有一组不一致，第一档就切不上去。
		f.fillParity(t, "x-scan", 6, 6) // 6 组全部不一致
		if _, _, err := f.svc.Stage(ctx, "x-scan", cutoverVer, 10, "ops"); err == nil ||
			!strings.Contains(err.Error(), "离线双跑存在不一致") {
			t.Fatalf("双跑不一致禁止上灰度，得到 %v", err)
		}

		// 重跑一批双跑（全一致）→ 首档放行：首档门槛只看「最新一批探针」，
		// 否则修好之后一次历史不一致会把第一档永久锁死。
		f.fillParity(t, "x-scan", 54, 0)
		if _, _, err := f.svc.Stage(ctx, "x-scan", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage 10: %v", err)
		}
		// 升档看总体一致率（全窗口）：首轮 6 组不一致仍在窗口里 → 54/60 = 0.90 不达标。
		f.advance(31 * time.Minute)
		f.fillRuns(t, "x-scan", 30, 0) // 成功率满分：拦着升档的只剩一致率
		var probes int64
		if err := f.db.Model(&db.SkillRun{}).Where("workflow_id = ?", ParityPrefix).Count(&probes).Error; err != nil {
			t.Fatalf("count probes: %v", err)
		}
		if probes != 120 {
			t.Fatalf("探针条数异常（首轮 6 组 + 重跑 54 组，各 ×2 版）= %d", probes)
		}
		if ev, err := f.svc.Evidence(ctx, "x-scan", cutoverVer); err != nil {
			t.Fatalf("evidence: %v", err)
		} else if ev.ParityTotal != 60 || ev.ParityMatch != 54 {
			t.Fatalf("一致率口径异常（60 组里应有 6 组不一致）: %+v", ev)
		} else if ev.LatestBatchTotal != 54 || ev.LatestBatchMatch != 54 {
			t.Fatalf("首档门槛应只看最新一批探针（54 组全一致）: %+v", ev)
		}
		if _, _, err := f.svc.Stage(ctx, "x-scan", cutoverVer, 50, "ops"); err == nil ||
			!strings.Contains(err.Error(), "一致率") {
			t.Fatalf("一致率不达标必须点名一致率，得到 %v", err)
		}
	})

	t.Run("探针入组不被组名长度破坏", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "x-scan", xScanTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		f.fillParity(t, "x-scan", 25, 0)
		if _, _, err := f.svc.Stage(ctx, "x-scan", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage 10: %v", err)
		}
		f.advance(31 * time.Minute)
		long := ParityProbeGroup(strings.Repeat("很长的批次号-", 12), 999, 1) // 远超 workflow_id 的 VARCHAR(64)
		for _, ver := range []string{baselineVer, cutoverVer} {
			if err := f.svc.RecordParityRun(ctx, "x-scan", ver, long, hash64("in-"+long), `{"count":1}`, true); err != nil {
				t.Fatalf("record probe (%s): %v", ver, err)
			}
		}
		ev, err := f.svc.Evidence(ctx, "x-scan", cutoverVer)
		if err != nil {
			t.Fatalf("evidence: %v", err)
		}
		if ev.ParityTotal != 26 || ev.ParityMatch != 26 {
			t.Fatalf("长组名的探针必须能正常入组比对（25 组基线 + 1 组长组名），得到 %+v", ev)
		}
	})

	t.Run("未跑完的探针组不算样本", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		for i := 0; i < 25; i++ {
			g := ParityProbeGroup(cutoverGroup, 900+i, 1)
			if err := f.svc.RecordParityRun(ctx, "like", baselineVer, g, hash64("in-"+g), `{"count":3}`, true); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
		ev, err := f.svc.Evidence(ctx, "like", cutoverVer)
		if err != nil {
			t.Fatalf("evidence: %v", err)
		}
		if ev.ParityTotal != 0 {
			t.Fatalf("单边组（只有旧实现）不能算可比对样本，得到 %+v", ev)
		}
	})

	t.Run("生产调用不混进一致率", func(t *testing.T) {
		f := newCutFixture(t)
		f.fillParity(t, "like", 3, 0)
		f.fillRuns(t, "like", 3, 0)
		var probes int64
		if err := f.db.Model(&db.SkillRun{}).Where("workflow_id = ?", ParityPrefix).Count(&probes).Error; err != nil {
			t.Fatalf("count probes: %v", err)
		}
		if probes != 6 {
			t.Fatalf("探针条数异常（3 组 × 2 版）= %d", probes)
		}
		ev, err := f.svc.Evidence(context.Background(), "like", cutoverVer)
		if err != nil {
			t.Fatalf("evidence: %v", err)
		}
		if ev.Samples != 3 || ev.ParityTotal != 3 {
			t.Fatalf("成功率与一致率的取数窗口互相污染: %+v", ev)
		}
	})
}

func TestCutoverRejectsUnknownSkillAndBadParams(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)

	if _, _, err := f.svc.Stage(ctx, "nosuch", cutoverVer, 10, "ops"); err == nil {
		t.Fatal("不存在的 skill 必须报错")
	}
	if _, err := f.svc.Target(ctx, "like", "  ", "ops", ""); err == nil {
		t.Fatal("空目标 addr 必须报错")
	}
	if _, err := f.svc.Target(ctx, "like", likeTarget+"/", "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	if row, _ := f.svc.Get(ctx, "like"); row.TargetAddr != likeTarget {
		t.Fatalf("目标 addr 应去掉尾部斜杠，得到 %q", row.TargetAddr)
	}
	if err := f.svc.RecordParityRun(ctx, "like", cutoverVer, "", hash64("x"), "{}", true); err == nil {
		t.Fatal("空组名必须报错")
	}
	if err := f.svc.RecordParityRun(ctx, "like", cutoverVer, "g1", "not-a-sha256", "{}", true); err == nil {
		t.Fatal("入参指纹长度不对必须报错")
	}
	if err := f.svc.RecordParityRun(ctx, "like", "", "g1", hash64("x"), "{}", true); err == nil {
		t.Fatal("探针缺版本号必须报错")
	}
}

func TestDecommissionWaitsForStableWindow(t *testing.T) {
	ctx := context.Background()

	t.Run("未切流时全部阻断", func(t *testing.T) {
		f := newCutFixture(t)
		dec := NewDecommission(f.db, f.svc.Config(), 24*time.Hour)
		dec.now = func() time.Time { return f.now }
		pl, err := dec.Evaluate(ctx, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if pl.Ready || len(pl.Services) != 2 {
			t.Fatalf("未切流时不该有任何服务可下线: %+v", pl)
		}
		for _, s := range pl.Services {
			if s.Action != ActionKeep || len(s.Blocked) == 0 {
				t.Fatalf("未切流的旧服务必须 keep + 阻断项: %+v", s)
			}
		}
	})

	t.Run("放量中禁止缩容", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		f.fillParity(t, "like", 25, 0)
		f.fillParity(t, "like", 25, 0)
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage 10: %v", err)
		}
		dec := NewDecommission(f.db, f.svc.Config(), 24*time.Hour)
		dec.now = func() time.Time { return f.now }
		pl, err := dec.Evaluate(ctx, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		var like *ServicePlan
		for i := range pl.Services {
			if pl.Services[i].Service == "pyreplay" {
				like = &pl.Services[i]
			}
		}
		if like == nil || like.Action != ActionKeep || !strings.Contains(strings.Join(like.Blocked, "；"), "未收尾") {
			t.Fatalf("放量中的旧服务必须保持在线并给出「未收尾」阻断项: %+v", like)
		}
	})

	t.Run("收尾后先降载再停服", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		f.fillParity(t, "like", 30, 0)
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage 10: %v", err)
		}
		f.fillRuns(t, "like", 30, 0) // 10% 档观察窗内的真实流量
		f.advance(31 * time.Minute)
		f.fillRuns(t, "like", 25, 0) // 100% 档观察窗内的真实流量
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 100, "ops"); err != nil {
			t.Fatalf("stage 100: %v", err)
		}
		if _, _, err := f.svc.Complete(ctx, "like", "ops"); err != nil {
			t.Fatalf("complete: %v", err)
		}

		dec := NewDecommission(f.db, f.svc.Config(), 24*time.Hour)
		dec.now = func() time.Time { return f.now }

		pl, err := dec.Evaluate(ctx, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		like := planOf(pl, "pyreplay")
		if like == nil {
			t.Fatalf("下线清单缺 pyreplay: %+v", pl)
		}
		if like.Action != ActionScaleDown || like.Ready() {
			t.Fatalf("刚收尾（观察窗未满）只能降载且必须带阻断项: %+v", like)
		}

		// 观察窗内旧链路仍有调用 = 存在没被网关管住的调用方（直连旧 addr / 老容器仍在跑），
		// 窗口再久也不许停 —— 停服会让这些调用直接失败。旧链路的流量落在旧实现版本上，
		// 所以记在 baselineVer 名下：它不进新实现的成功率证据，只进「旧链路残余调用」计数。
		f.fillRunsVer(t, "like", baselineVer, 3, 0)
		f.advance(25 * time.Hour)
		pl, err = dec.Evaluate(ctx, nil)
		if err != nil {
			t.Fatalf("evaluate later: %v", err)
		}
		like = planOf(pl, "pyreplay")
		if like.Action != ActionScaleDown || like.Ready() ||
			!strings.Contains(strings.Join(like.Blocked, "；"), "旧链路仍有") {
			t.Fatalf("收尾后观察窗内旧链路还有流量，禁止停服: %+v", like)
		}

		// 「清掉残余调用方」= 旧实现不再被调用。旧实现名下的记录只有上面那 3 条（新实现的
		// 生产样本是切流目标的证据，放量/收尾护栏都要读，一条都不能动）。
		res := f.db.Where("skill_name = ? AND version = ? AND workflow_id <> ?",
			"like", baselineVer, ParityPrefix).Delete(&db.SkillRun{})
		if res.Error != nil {
			t.Fatalf("clear residual calls: %v", res.Error)
		}
		if res.RowsAffected != 3 { // 只清生产调用；双跑探针（每版 30 条）是证据，不许动
			t.Fatalf("收尾后的残余调用应是 3 条，删掉 %d 条", res.RowsAffected)
		}
		pl, err = dec.Evaluate(ctx, nil)
		if err != nil {
			t.Fatalf("evaluate clean: %v", err)
		}
		like = planOf(pl, "pyreplay")
		if like.Action != ActionStop || like.Replicas != 0 || !like.Ready() {
			t.Fatalf("稳定观察满窗口且旧链路零调用后 pyreplay 应可停服: %+v", like)
		}
		if x := planOf(pl, "pycraper"); x == nil || x.Action != ActionKeep {
			t.Fatalf("x-scan 没切流，pycraper 必须保持在线: %+v", x)
		}
		if pl.Ready {
			t.Fatal("只要有一个服务被阻断，整体计划就不该 Ready")
		}
	})

	// 停服准入路径上的「取数失败」必须判阻断，绝不能被读成「旧链路零调用」。
	t.Run("旧链路计数取数失败不许当成零调用", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		f.fillParity(t, "like", 30, 0)
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage 10: %v", err)
		}
		f.fillRuns(t, "like", 30, 0)
		f.advance(31 * time.Minute)
		f.fillRuns(t, "like", 25, 0)
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 100, "ops"); err != nil {
			t.Fatalf("stage 100: %v", err)
		}
		if _, _, err := f.svc.Complete(ctx, "like", "ops"); err != nil {
			t.Fatalf("complete: %v", err)
		}
		f.advance(25 * time.Hour) // 观察窗已满：这是唯一能走到「停服」的形态

		dec := NewDecommission(f.db, f.svc.Config(), 24*time.Hour)
		dec.now = func() time.Time { return f.now }

		// 把 skill_runs 打掉：所有读 skill_runs 的取数（旧链路计数 + 质量证据）全部失败。
		// 断言的是「取数失败一律判阻断」这条不变量 —— 任何一个取数点把失败当成「零调用 /
		// 零证据继续往下判」，都会让这份清单在 DB 抖动时给出「可以停服」的结论。
		if err := f.db.Exec("DROP TABLE skill_runs").Error; err != nil {
			t.Fatalf("drop skill_runs: %v", err)
		}
		pl, err := dec.Evaluate(ctx, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		like := planOf(pl, "pyreplay")
		if like == nil {
			t.Fatalf("下线清单缺 pyreplay: %+v", pl)
		}
		if like.Action == ActionStop || like.Ready() {
			t.Fatalf("计数取数失败绝不能被当成「旧链路零调用」而放行停服: %+v", like)
		}
		if !strings.Contains(strings.Join(like.Blocked, "；"), "取数失败") {
			t.Fatalf("必须给出「旧链路调用数取数失败」这条阻断项: %+v", like)
		}
		if pl.Ready {
			t.Fatal("有服务因取数失败被阻断时整体计划不该 Ready")
		}
	})

	t.Run("回滚后旧服务不许停", func(t *testing.T) {
		f := newCutFixture(t)
		if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
			t.Fatalf("target: %v", err)
		}
		f.fillParity(t, "like", 25, 0)
		f.fillParity(t, "like", 25, 0)
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
			t.Fatalf("stage: %v", err)
		}
		if err := f.svc.Rollback(ctx, "like", "探针不一致", "ops"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		dec := NewDecommission(f.db, f.svc.Config(), time.Hour)
		dec.now = func() time.Time { return f.now.Add(100 * time.Hour) }
		pl, err := dec.Evaluate(ctx, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		like := planOf(pl, "pyreplay")
		if like.Action != ActionKeep || !strings.Contains(strings.Join(like.Blocked, "；"), "回滚") {
			t.Fatalf("切流回滚后旧服务必须保持在线: %+v", like)
		}
	})
}

func planOf(pl *Plan, service string) *ServicePlan {
	for i := range pl.Services {
		if pl.Services[i].Service == service {
			return &pl.Services[i]
		}
	}
	return nil
}

func TestDecommissionLegacyCallCount(t *testing.T) {
	f := newCutFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	f.fillParity(t, "like", 4, 0) // 4 组探针（8 条）
	f.fillRuns(t, "like", 4, 0)   // + 4 条生产调用
	dec := NewDecommission(f.db, f.svc.Config(), 24*time.Hour)
	n, err := dec.LegacyCallCount(context.Background(), "like", f.now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("legacy call count: %v", err)
	}
	if n != 4 {
		t.Fatalf("生产调用计数必须排除对比探针，得到 %d", n)
	}
}

// TestCutoverRollbackZeroesTrafficInDB 钉住「流量真归零」这件事落在**库里**，而不是只落在
// 内存对象上：SkillVersion.TrafficSplit 带 gorm `default:100`，GORM 的 INSERT/UPDATE 会跳过
// 带 default 标签的零值字段 → 库里留旧值（甚至 DB 默认 100），而网关只认库里的数
// （ResolveLive 按 status IN (live,gray) AND traffic_split > 0 加权路由）。
//
// 回滚是最典型的「写零」路径：切流版本必须退出路由（0% + retired）。这里每一步都从库里重读，
// 并且断言「参与路由的版本流量之和恒为 100」——任何一次「内存归零、库里没归零」都会让它破。
func TestCutoverRollbackZeroesTrafficInDB(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	f.fillParity(t, "like", 6, 0)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("stage 10: %v", err)
	}

	// routedTotal 返回库里「正在参与路由」的版本流量之和。
	routedTotal := func() int {
		t.Helper()
		var sum int
		if err := f.db.Model(&db.SkillVersion{}).
			Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND status IN ? AND traffic_split > 0",
				"like", []string{"live", "gray"}).
			Select("COALESCE(SUM(traffic_split), 0)").Scan(&sum).Error; err != nil {
			t.Fatalf("sum traffic: %v", err)
		}
		return sum
	}
	if n := routedTotal(); n != 100 {
		t.Fatalf("放量后在路由版本流量之和应为 100，得到 %d", n)
	}

	if err := f.svc.Rollback(ctx, "like", "线上劣化，切回旧实现", "ops"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 0 || st != "retired" {
		t.Fatalf("回滚后切流版本必须 0%%/retired（库里），得到 %d/%s", n, st)
	}
	if n, st := f.traffic(t, "like", baselineVer); n != 100 || st != "live" {
		t.Fatalf("回滚后基线必须满血（%d/%s）", n, st)
	}
	if n := routedTotal(); n != 100 {
		t.Fatalf("回滚后在路由版本流量之和应回到 100，得到 %d（说明有版本带着残留流量在路由）", n)
	}

	// 用裸 SQL 复核一遍：库里真的归零（不是 GORM 模型读回来的内存值）。
	if n := f.trafficRaw(t, "like", cutoverVer); n != 0 {
		t.Fatalf("回滚后库里的 traffic_split 必须是 0，得到 %d", n)
	}

	// 回滚后重放第一档：复用的必须是「本切流登记过」的那一行，且落库档位恰好是 10。
	f.advance(31 * time.Minute)
	f.fillParity(t, "like", 54, 0) // 重放前重跑一批双跑（首档门槛 = 最新一批全一致）	f.fillParity(t, "like", 54, 0) // 重放前重跑一批双跑（首档门槛 = 最新一批全一致）
	f.fillRuns(t, "like", 30, 0)   // 成功率样本：旧实现这一窗口的真实调用记录
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("回滚后重放第一档: %v", err)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 10 || st != "gray" {
		t.Fatalf("重放后切流版本应 10%%/gray（库里），得到 %d/%s", n, st)
	}
	if n := routedTotal(); n != 100 {
		t.Fatalf("重放后流量之和应为 100，得到 %d", n)
	}
}

// TestCutoverReplayAfterRollbackUsesParityGate 钉住「回滚 → 修复 → 重切」这条链在生产上真的走得通。
//
// 为什么必须单独钉（既有全链路测试会漏掉它）：全链路测试里，回滚前灌的生产样本时间戳恰好等于
// 回滚时刻，回滚把 stage_at 也写成同一时刻，`created_at >= stage_at` 于是成立 —— 样本被算进
// 新窗口，「按升档门槛要生产样本」这条错误分支被测试自己造出来的数据掩盖了。
//
// 真实生产里不可能出现那种数据：回滚已把目标版本置 retired + traffic_split=0，网关不再路由到它，
// 该版本**永远不会再有** stage_at 之后的生产样本。若回滚态/零流量仍按 GateReasons 判
// （要求 ≥MinSamples 条生产样本），首档重放就永远停在「成功率样本不足」，切流单向锁死。
// 回滚后的第一档必须回到首档口径：离线双跑探针（FirstStageReasons）。
func TestCutoverReplayAfterRollbackUsesParityGate(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	f.fillParity(t, "like", 6, 0)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("stage 10: %v", err)
	}
	// 第一档窗口内的真实生产调用（新实现接的流量）：时间戳必须落在 stage_at 之前、回滚之前。
	// 这是生产的形状 —— 回滚之后新实现 retired/0，不再产生样本，「回滚时刻之后样本恒为 0」。
	f.advance(10 * time.Minute)
	f.fillRuns(t, "like", 30, 0)

	f.advance(10 * time.Minute)
	if err := f.svc.Rollback(ctx, "like", "一致率跌破阈值", "ops"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// 时间推进过观察窗口；这期间新实现零流量（retired/0）→ 零生产样本，探针也重跑一批。
	f.advance(31 * time.Minute)
	f.fillParity(t, "like", 6, 0)

	var samples int64
	if err := f.db.Model(&db.SkillRun{}).
		Where("skill_name = ? AND version = ? AND workflow_id <> ? AND created_at >= ?",
			"like", cutoverVer, ParityPrefix, *f.svcRow(t, ctx).StageAt).
		Count(&samples).Error; err != nil {
		t.Fatalf("count post-rollback samples: %v", err)
	}
	if samples != 0 {
		t.Fatalf("回滚后目标版本不该再有生产样本（否则本测试就掩盖了 bug）：%d", samples)
	}

	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("回滚后重放第一档必须只看离线双跑，得到 %v", err)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 10 || st != "gray" {
		t.Fatalf("重放后应 10%%/gray，得到 %d/%s", n, st)
	}
}

// TestDecommissionCountsProbePrefixOnly 钉住旧链路计数的取数形状：观察窗内的生产调用 =
// 「workflow_id 不是探针标记」的那些行（计数走 LIKE 'parity:%' 前缀匹配，不是等值比较）。
// 这保证「计数只排除离线探针、把每一个非目标版本都算成旧实现流量」这条口径可被单测直接验证。
func TestDecommissionCountsProbePrefixOnly(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	dec := NewDecommission(f.db, f.svc.Config(), 24*time.Hour)
	dec.now = func() time.Time { return f.now }

	start := f.now
	f.fillRunsVer(t, "like", baselineVer, 4, 0) // 旧实现接了 4 次生产流量
	f.fillParity(t, "like", 3, 0)               // 探针（每版 3 条）必须不计入

	n, err := dec.LegacyCallCount(ctx, "like", start)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Fatalf("旧链路计数应只数生产调用（4），得到 %d", n)
	}
	if n, err := dec.LegacyCallCount(ctx, "like", f.now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("窗口之后的调用不在观察窗内，应为 0，得到 %d / %v", n, err)
	}
}

// TestCutoverRollbackRestoresRecordedLegacyVersion 钉住回滚恢复的是 skill_cutover.legacy_version
// （切流时被切下来的那一版），不是「除目标版本外 id 最小的历史版本」。
//
// 为什么必须单独钉：S1 灰度（5→50→100）每轮都会在 skill_versions 里留下版本行，一个 skill
// 攒出 v1/v1.1/v2 是常态。lookupLegacyVersion 按 id ASC 取的是**最老**的一行 —— 只认它的话，
// 回滚会把生产流量切到几个版本前、早已废弃的实现上，而不是刚刚被切下来的那一版。
func TestCutoverRollbackRestoresRecordedLegacyVersion(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)

	// 造一个「多历史版本」的 like：v1（老基线，已被 S1 灰度切下）+ v1.1（当前 live 基线）。
	var skill db.Skill
	if err := f.db.Where("name = ?", "like").First(&skill).Error; err != nil {
		t.Fatalf("skill: %v", err)
	}
	cfg, err := json.Marshal(map[string]any{"addr": legacyLike})
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	if err := f.db.Model(&db.SkillVersion{}).
		Where("skill_id = ? AND version = ?", skill.ID, baselineVer).
		Updates(map[string]any{"traffic_split": 0, "status": "retired"}).Error; err != nil {
		t.Fatalf("retire v1: %v", err)
	}
	if err := f.db.Create(&db.SkillVersion{SkillID: skill.ID, Version: "v1.1",
		Image: "registry.local/like:v1.1", TrafficSplit: 100, Status: "live", Config: string(cfg), CreatedAt: f.now}).Error; err != nil {
		t.Fatalf("create v1.1: %v", err)
	}

	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	f.fillParity(t, "like", 6, 0)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("stage 10: %v", err)
	}
	row, err := f.svc.Get(ctx, "like")
	if err != nil || row.LegacyVer != "v1.1" {
		t.Fatalf("切流必须记下真正在承接流量的旧版本 v1.1，得到 %+v (%v)", row, err)
	}

	if err := f.svc.Rollback(ctx, "like", "线上新实现劣化", "ops"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n, st := f.traffic(t, "like", "v1.1"); n != 100 || st != "live" {
		t.Fatalf("回滚必须恢复 v1.1 到 100%%/live，得到 %d/%s", n, st)
	}
	// 最老的 v1 绝不能被拉回线上。
	if n, st := f.traffic(t, "like", baselineVer); n != 0 || st != "retired" {
		t.Fatalf("回滚不许把最老的历史版本（v1）恢复上线，得到 %d/%s", n, st)
	}
}

// TestCutoverApplyTrafficSumsTo100WithMultipleLegacyVersions 钉住「承接剩余流量的旧版本 ≥2 个」时
// 流量分配之和必须恰好等于 remaining（各版本合计恒为 100）。
//
// 为什么必须单独钉：S1 灰度期间同一 skill 同时有 live(95%) + gray(5%) 两行带流量是常态
// （cd.go 的 startGray 就这么写）。旧实现按「现有流量等比」承接 remaining，末位必须只补
// 「还剩多少没分」；末位直接赋 remaining 会让 Σ = remaining*(1+Σ非末位权重/total) > remaining。
// 网关 ResolveLive 按各行 traffic_split 加权路由 → 超发抢量（例：others=[95,5]、traffic=10
// 时旧实现合计 175% + 新实现 10% = 185%）。
func TestCutoverApplyTrafficSumsTo100WithMultipleLegacyVersions(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		others  []int // 各旧版本放量前的流量（顺序 = id 升序，末位是补余数的那一个）
		traffic int
	}{
		{"95+5 放 10", []int{95, 5}, 10},
		{"95+5 放 50", []int{95, 5}, 50},
		{"90+10 放 10", []int{90, 10}, 10},
		{"60+30+10 放 10", []int{60, 30, 10}, 10},
		{"99+1 放 100", []int{99, 1}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCutFixture(t)
			var skill db.Skill
			if err := f.db.Where("name = ?", "like").First(&skill).Error; err != nil {
				t.Fatalf("skill: %v", err)
			}
			// 撤掉 fixture 自带的基线，改成用例指定的多版本分布。
			if err := f.db.Where("skill_id = ?", skill.ID).Delete(&db.SkillVersion{}).Error; err != nil {
				t.Fatalf("clear versions: %v", err)
			}
			cfg, err := json.Marshal(map[string]any{"addr": legacyLike})
			if err != nil {
				t.Fatalf("marshal cfg: %v", err)
			}
			for _, n := range tc.others {
				st := "live"
				if n == 0 {
					st = "gray" // 零流量行不进路由池（applyTraffic 只取 traffic_split>0）
				}
				if err := f.db.Create(&db.SkillVersion{SkillID: skill.ID, Version: "old-" + itoa(n),
					Image: "registry.local/like:v1", TrafficSplit: n, Status: st, Config: string(cfg), CreatedAt: f.now}).Error; err != nil {
					t.Fatalf("seed version %d: %v", n, err)
				}
			}

			v := &db.SkillVersion{SkillID: skill.ID, Version: cutoverVer, Image: "registry.local/like:v1",
				TrafficSplit: 0, Status: "gray", Config: string(cfg), CreatedAt: f.now}
			if err := f.db.Select("skill_id", "version", "image", "config", "traffic_split", "status", "created_at").
				Create(v).Error; err != nil {
				t.Fatalf("create cutover version: %v", err)
			}
			if _, err := f.svc.applyTraffic(ctx, skill.ID, v, tc.traffic); err != nil {
				t.Fatalf("applyTraffic: %v", err)
			}

			var sum int
			if err := f.db.Model(&db.SkillVersion{}).
				Where("skill_id = ? AND status IN ? AND traffic_split > 0", skill.ID, []string{"live", "gray"}).
				Select("COALESCE(SUM(traffic_split), 0)").Scan(&sum).Error; err != nil {
				t.Fatalf("sum: %v", err)
			}
			if sum != 100 {
				t.Fatalf("在路由版本的流量之和必须恰好 100（切流 %d%% + 旧实现承接 %d%%），得到 %d",
					tc.traffic, 100-tc.traffic, sum)
			}
			// 旧实现这一侧也必须恰好承接 remaining（不能被多分）。
			var legacySum int
			if err := f.db.Model(&db.SkillVersion{}).
				Where("skill_id = ? AND version <> ? AND traffic_split > 0", skill.ID, cutoverVer).
				Select("COALESCE(SUM(traffic_split), 0)").Scan(&legacySum).Error; err != nil {
				t.Fatalf("sum legacy: %v", err)
			}
			if legacySum != 100-tc.traffic {
				t.Fatalf("旧实现应合计承接 %d%%，得到 %d%%", 100-tc.traffic, legacySum)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// rampToCompleted 把 like 一路放量到 100% 并收尾（返回收尾后的切流行）。
func rampToCompleted(t *testing.T, f *cutFixture, ctx context.Context) *db.SkillCutover {
	t.Helper()
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	f.fillParity(t, "like", 30, 0)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("stage 10: %v", err)
	}
	f.fillRuns(t, "like", 30, 0)
	f.advance(31 * time.Minute)
	f.fillRuns(t, "like", 25, 0)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 100, "ops"); err != nil {
		t.Fatalf("stage 100: %v", err)
	}
	if _, _, err := f.svc.Complete(ctx, "like", "ops"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	row, err := f.svc.Get(ctx, "like")
	if err != nil || row == nil {
		t.Fatalf("get: %v", err)
	}
	return row
}

// TestCutoverStageBlockedAfterComplete 钉住「已收尾禁止再改流量」（ErrCutoverCompleted 定义就是
// 给这条用的，此前只有 Complete 自己在用）。
//
// 收尾之后 skills.addr 已收敛到新实现、其余版本全部 retired/0：此时 Stage(10) 会让 applyTraffic
// 找不到任何承接剩余流量的旧版本，切流版本成为路由池里唯一一行 —— 网关按各行流量归一加权，线上
// 仍 100% 打新实现，而 skill_cutover 记着「10%」。状态机与真实流量彻底脱节，随后的 Complete 又因
// traffic < 100 被拒，只能逐档放量绕回来。
func TestCutoverStageBlockedAfterComplete(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	row := rampToCompleted(t, f, ctx)
	if row.Status != CutoverCompleted {
		t.Fatalf("前置条件：必须已收尾，得到 %+v", row)
	}
	addrAfterComplete := f.addrOf(t, "like")

	for _, stage := range []int{10, 50, 100} {
		if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, stage, "ops"); !errors.Is(err, ErrCutoverCompleted) {
			t.Fatalf("收尾后放量到 %d%% 必须被 ErrCutoverCompleted 拦下，得到 %v", stage, err)
		}
	}
	// 拒绝必须是「什么都没改」：档位与版本行流量保持收尾后的状态。
	after, err := f.svc.Get(ctx, "like")
	if err != nil || after == nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != CutoverCompleted || after.Traffic != 100 {
		t.Fatalf("被拒的 Stage 不许改动切流行: %+v", after)
	}
	if n, st := f.traffic(t, "like", cutoverVer); n != 100 || st != "live" {
		t.Fatalf("被拒的 Stage 不许把切流版本降回 gray，得到 %d/%s", n, st)
	}
	if f.addrOf(t, "like") != addrAfterComplete {
		t.Fatalf("被拒的 Stage 不许动 skills.addr: %s", f.addrOf(t, "like"))
	}
}

// TestCutoverRollbackAfterCompleteRestoresBaseAddr 钉住「收尾后回滚」把基线 addr 一起回退。
//
// Complete 会把 skills.addr 收敛到目标 addr，而基线 addr 是 S5 deploy.VersionManager.baseService
// 的来源（起版本容器 / 还原 config.addr 都读它）。Complete 的错误文案明确把人导向「要重切请先回滚」，
// 所以这条路径必然被走到：不回退 addr 的话，回滚窗口里任何一次 deployctl version up 都按新实现的
// compose 服务与环境起容器，与线上实际承接流量的旧实现错位。
func TestCutoverRollbackAfterCompleteRestoresBaseAddr(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	baseBefore := f.addrOf(t, "like") // 旧实现 addr（fixture 的基线）
	row := rampToCompleted(t, f, ctx)
	_ = row
	if got := f.addrOf(t, "like"); got != likeTarget {
		t.Fatalf("收尾后基线 addr 应收敛到目标 addr，得到 %s", got)
	}

	if err := f.svc.Rollback(ctx, "like", "收尾后发现新实现劣化", "ops"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := f.addrOf(t, "like"); got != baseBefore {
		t.Fatalf("收尾后回滚必须把 skills.addr 改回旧实现（基线是 S5 起版本容器的依据），得到 %s", got)
	}
	if n, st := f.traffic(t, "like", baselineVer); n != 100 || st != "live" {
		t.Fatalf("回滚后旧实现必须满血在线（%d/%s）", n, st)
	}
	after, err := f.svc.Get(ctx, "like")
	if err != nil || after == nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != CutoverRolledBack || after.Traffic != 0 {
		t.Fatalf("回滚后切流行异常: %+v", after)
	}
	if after.CompletedAt != nil {
		t.Fatalf("收尾已撤销，稳定观察窗起点必须清空，得到 %v", after.CompletedAt)
	}
}

// TestCutoverRollbackBeforeCompleteKeepsBaseAddr 反向对照：未收尾的回滚绝不改基线 addr。
func TestCutoverRollbackBeforeCompleteKeepsBaseAddr(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	baseBefore := f.addrOf(t, "like")
	f.fillParity(t, "like", 6, 0)
	if _, _, err := f.svc.Stage(ctx, "like", cutoverVer, 10, "ops"); err != nil {
		t.Fatalf("stage 10: %v", err)
	}
	if got := f.addrOf(t, "like"); got != baseBefore {
		t.Fatalf("放量阶段绝不该动基线 addr，得到 %s", got)
	}
	if err := f.svc.Rollback(ctx, "like", "首档就劣化", "ops"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := f.addrOf(t, "like"); got != baseBefore {
		t.Fatalf("未收尾的回滚不该改基线 addr（本来就是旧实现），得到 %s", got)
	}
}

// TestCutoverRollbackBeforeRampTouchesNoVersion 钉住「只登记了 target、从未放量」的回滚：
// 一个版本行都不许动。
//
// 为什么必须单独钉（既有的同名场景测试会漏掉）：那个 fixture 只有一个版本行，兜底查询
// （lookupLegacyVersion 按 id ASC 取「除切流版本外最早的行」）取到的恰好就是当前 live 基线，
// 「恢复它」= 原样写回，看不出任何问题。真实 skill 会随 S1 灰度不断累积版本行，于是同一段兜底
// 逻辑取到的是**最老**的那一行 —— 一次「上线前撤掉登记」的回滚会把几个版本前早已废弃的实现
// 重新拉成 live/100%，与当前 live 基线各带 100% 流量（网关按流量归一 ≈ 一半真实流量打到废弃
// 实现），而切流行已 rolled_back、Complete 被禁止，状态机不会自愈。
func TestCutoverRollbackBeforeRampTouchesNoVersion(t *testing.T) {
	ctx := context.Background()
	f := newCutFixture(t)
	skillID := f.skillID(t, "like")
	cfg, err := json.Marshal(map[string]any{"addr": legacyLike})
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}

	// v0 = 比基线更早的废弃版本（id 更小 = 兜底查询按 id ASC 会挑中的那一行）。
	// 用裸 SQL 灌并显式写 traffic_split=0：GORM 的 Create 对带 default 标签的零值字段不下发
	// 该列（库里会落 DB 默认 100），那样 fixture 就成了「全员 100%」的库 —— 「把 v0 拉到 100%」
	// 与「什么都没改」在断言上完全一样，测试会假绿。
	if err := f.db.Exec(
		"INSERT INTO skill_versions (skill_id, version, image, traffic_split, status, config, created_at)"+
			" VALUES (?, 'v0', 'registry.local/like:v0', 0, 'retired', ?, ?)",
		skillID, string(cfg), f.now).Error; err != nil {
		t.Fatalf("seed v0: %v", err)
	}
	if n := f.trafficRaw(t, "like", "v0"); n != 0 {
		t.Fatalf("前置条件：v0 必须是零流量的历史版本，得到 %d", n)
	}
	if n := f.trafficRaw(t, "like", baselineVer); n != 100 {
		t.Fatalf("前置条件：基线必须 live/100%%，得到 %d", n)
	}

	if _, err := f.svc.Target(ctx, "like", likeTarget, "ops", ""); err != nil {
		t.Fatalf("target: %v", err)
	}
	row, err := f.svc.Get(ctx, "like")
	if err != nil || row == nil || row.TargetVer != "" {
		t.Fatalf("前置条件：必须只登记 target、从未放量: %+v (%v)", row, err)
	}

	if err := f.svc.Rollback(ctx, "like", "上线前撤掉登记", "ops"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if n, st := f.traffic(t, "like", baselineVer); n != 100 || st != "live" {
		t.Fatalf("当前基线必须原样在线（%d/%s）", n, st)
	}
	if n, st := f.traffic(t, "like", "v0"); n != 0 || st != "retired" {
		t.Fatalf("从未放量就回滚，绝不许把更老的历史版本拉回线上，得到 v0=%d/%s", n, st)
	}
	after, err := f.svc.Get(ctx, "like")
	if err != nil || after == nil || after.Status != CutoverRolledBack {
		t.Fatalf("切流行状态异常: %+v (%v)", after, err)
	}
}
