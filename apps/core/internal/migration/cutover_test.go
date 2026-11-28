// M4-1（工作项 wi-1795773674-9be97c4b / wi-1795489658-2d59）跨服务切流单测。
//
// 覆盖验收标准「skill 网关 x-scan / like 的 addr 指向 social，traffic_split 10%→50%→100%
// 灰度放量」的可单测部分：
//   - 档位白名单（只有 10/50/100 是合法档位，0/7/30/101/-10 全拒）；
//   - 护栏缺省值（时长 / 样本量 / 一致率 / 成功率）与逐条拒绝原因；
//   - 一致率判定 ParityMatch：组内必须两版都有、都成功、结果摘要指纹一致；
//   - 探针摘要哈希的读写口径（ParityInputHash / ParitySummaryHash）与「零证据不放量」；
//   - 切流写操作必须接 DB（无 DB = 报错，绝不静默成功）。
//
// CI 安全：MySQL / Redis / Qdrant / 浏览器一个都不起。
package migration

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"

	"github.com/wetee/ai-workflow/internal/db"
)

func TestValidStage(t *testing.T) {
	for _, ok := range []int{10, 50, 100} {
		if !ValidStage(ok) {
			t.Fatalf("档位 %d 必须合法（10%%→50%%→100%%）", ok)
		}
	}
	// 0 是「回滚后/未放量」的状态，不是可请求的放量档位；其余数字一律拒绝。
	for _, bad := range []int{0, 1, 7, 20, 30, 49, 51, 99, 101, -10} {
		if ValidStage(bad) {
			t.Fatalf("档位 %d 必须被拒绝（只允许逐档 %v）", bad, CutoverStages)
		}
	}
	if !strings.Contains(StagesLabel(), "10") || !strings.Contains(StagesLabel(), "100") {
		t.Fatalf("StagesLabel 要能告诉操作者合法档位，得到 %q", StagesLabel())
	}
}

func TestParityProbeFingerprints(t *testing.T) {
	// 入组键 = sha256(组标识) + sha256(版本号)：两段定长指纹，绝不因组名长度而截断。
	key := ParityProbeKey("batch-2026-12-05/用例-0001", "v2")
	if len(key) != 128 {
		t.Fatalf("入组键必须是 128 个 hex 字符，得到长度 %d", len(key))
	}
	if key[:64] != sha256hex([]byte("batch-2026-12-05/用例-0001")) || key[64:] != sha256hex([]byte("v2")) {
		t.Fatal("入组键必须由「组标识指纹 + 版本号指纹」拼成（读侧要能只按前缀分组）")
	}
	if ParityProbeKey("g1", "v2") == ParityProbeKey("g2", "v2") {
		t.Fatal("不同组必须落进不同分组键（否则两个批次的一致率被并成一堆）")
	}
	if ParityProbeKey("g1", "v1") == ParityProbeKey("g1", "v2") {
		t.Fatal("同组不同版本必须落进不同版本键")
	}

	// 结果摘要指纹：空摘要 = 无指纹（探针没带结果不能当成一致）。
	if ParitySummaryHash("") != "" {
		t.Fatal("空摘要必须得空指纹")
	}
	if ParitySummaryHash(`{"count":3}`) != sha256hex([]byte(`{"count":3}`)) {
		t.Fatal("摘要指纹口径必须稳定（运维脚本与 RecordParityRun 同算法）")
	}
	if ParitySummaryHash(`{"count":3}`) == ParitySummaryHash(`{"count":4}`) {
		t.Fatal("结果不同必须指纹不同")
	}
}

func TestParityMatch(t *testing.T) {
	// ParityMatch 的入参 key = 版本号指纹，记录的 Error 列 = 结果摘要指纹。
	ok := func(version, summary string) db.SkillRun {
		return db.SkillRun{Version: version, Status: "ok", Error: ParitySummaryHash(summary)}
	}

	if ParityMatch(map[string]db.SkillRun{"v1": ok("v1", `{"count":3}`)}) {
		t.Fatal("单边组（探针没跑完）不能算一致")
	}
	if ParityMatch(nil) {
		t.Fatal("空组不能算一致")
	}
	if !ParityMatch(map[string]db.SkillRun{"v1": ok("v1", `{"count":3}`), "v2": ok("v2", `{"count":3}`)}) {
		t.Fatal("同入参同结果 = 一致")
	}
	if ParityMatch(map[string]db.SkillRun{"v1": ok("v1", `{"count":3}`), "v2": ok("v2", `{"count":4}`)}) {
		t.Fatal("结果摘要不同必须判不一致")
	}
	if ParityMatch(map[string]db.SkillRun{
		"v1": ok("v1", `{"count":3}`),
		"v2": {Version: "v2", Status: "fail", Error: ParitySummaryHash(`{"count":3}`)},
	}) {
		t.Fatal("组内任一条失败必须判不一致（失败没有可比对的结果）")
	}
	if ParityMatch(map[string]db.SkillRun{"v1": ok("v1", ""), "v2": ok("v2", "")}) {
		t.Fatal("结果摘要缺失不能当成一致（探针没带结果）")
	}
	// 一致率只看结果，不看延迟：同一结果 + 不同延迟必须仍判一致。
	a := ok("v1", `{"count":3}`)
	b := ok("v2", `{"count":3}`)
	latA, latB := int64(120), int64(9000)
	a.LatencyMs, b.LatencyMs = &latA, &latB
	if !ParityMatch(map[string]db.SkillRun{"v1": a, "v2": b}) {
		t.Fatal("延迟绝不能进一致率口径（同结果必须判一致）")
	}
}

func TestCutoverConfigDefaults(t *testing.T) {
	cfg := CutoverConfig{}.WithDefaults()
	if cfg.Actor == "" {
		t.Fatal("缺省 actor 不能为空（审计留痕要有操作者）")
	}
	if cfg.MinStageInterval <= 0 || cfg.MinSamples <= 0 {
		t.Fatalf("时长/样本护栏缺省值异常: %+v", cfg)
	}
	if cfg.ParityMin <= 0 || cfg.ParityMin > 1 || cfg.SuccessMin <= 0 || cfg.SuccessMin > 1 {
		t.Fatalf("比率阈值必须在 (0,1]: %+v", cfg)
	}
	if cfg.SuccessMin < cfg.ParityMin {
		t.Fatalf("成功率阈值不该松于一致率（成功率是硬指标）: %+v", cfg)
	}

	// 切流行上的阈值覆盖缺省；0 = 未设置，沿用缺省。
	row := &db.SkillCutover{ParityMin: 0.99, Traffic: 50}
	eff := cfg.effective(row)
	if eff.ParityMin != 0.99 {
		t.Fatalf("行上阈值应覆盖缺省，得到 %v", eff.ParityMin)
	}
	if eff.SuccessMin != cfg.SuccessMin || eff.MinSamples != cfg.MinSamples {
		t.Fatalf("未设置的阈值应沿用缺省: %+v", eff)
	}
	if eff.MinStageInterval != cfg.MinStageInterval {
		t.Fatal("时长护栏缺省被意外覆盖")
	}
	if got := cfg.effective(nil); got.ParityMin != cfg.ParityMin {
		t.Fatal("nil 行应等价于全缺省")
	}
}

func TestEvidenceGateReasons(t *testing.T) {
	cfg := CutoverConfig{MinSamples: 20, ParityMin: 0.9, SuccessMin: 0.95}.WithDefaults()

	// 零证据 = 绝不放量（没有证据的「零失败」什么都证明不了）。
	zero := Evidence{}
	if zero.Satisfied(cfg) {
		t.Fatal("零证据必须判不达标")
	}
	reasons := strings.Join(zero.GateReasons(cfg), "；")
	if !strings.Contains(reasons, "样本不足") {
		t.Fatalf("零证据的原因要点名样本不足，得到 %q", reasons)
	}

	// 样本够、成功率不达标（100 条里 10 条失败 = 0.90 < 0.95）。
	mixed := Evidence{Samples: 100, OKSamples: 90, SuccessRate: 0.9, ParityTotal: 100, ParityMatch: 100, ParityRate: 1}
	if mixed.Satisfied(cfg) {
		t.Fatal("成功率不达标必须判不达标")
	}
	reasons = strings.Join(mixed.GateReasons(cfg), "；")
	if !strings.Contains(reasons, "成功率") {
		t.Fatalf("原因里要点名成功率，得到 %q", reasons)
	}

	// 一致率不达标（100 组里 20 组不一致 = 0.80 < 0.90）。
	parity := Evidence{Samples: 100, OKSamples: 100, SuccessRate: 1, ParityTotal: 100, ParityMatch: 80, ParityRate: 0.8}
	if parity.Satisfied(cfg) {
		t.Fatal("一致率不达标必须判不达标")
	}
	reasons = strings.Join(parity.GateReasons(cfg), "；")
	if !strings.Contains(reasons, "一致率") {
		t.Fatalf("原因里要点名一致率，得到 %q", reasons)
	}

	// 阈值边界（门槛是「严格大于」，整数交叉相乘判定，不受浮点舍入影响）：
	// 一致率刚好等于阈值 → 拦；刚过一点 → 放。成功率同理。
	atParity := Evidence{Samples: 100, OKSamples: 100, SuccessRate: 1, ParityTotal: 100, ParityMatch: 90, ParityRate: 0.9}
	if atParity.Satisfied(cfg) || !strings.Contains(strings.Join(atParity.GateReasons(cfg), "；"), "一致率") {
		t.Fatalf("一致率刚好踩到阈值必须拦下: %+v", atParity.GateReasons(cfg))
	}
	overParity := Evidence{Samples: 100, OKSamples: 100, SuccessRate: 1, ParityTotal: 1000, ParityMatch: 901, ParityRate: 0.901}
	if !overParity.Satisfied(cfg) {
		t.Fatalf("一致率刚过阈值必须放行: %+v", overParity.GateReasons(cfg))
	}
	atSuccess := Evidence{Samples: 100, OKSamples: 95, SuccessRate: 0.95, ParityTotal: 100, ParityMatch: 100, ParityRate: 1}
	if atSuccess.Satisfied(cfg) || !strings.Contains(strings.Join(atSuccess.GateReasons(cfg), "；"), "成功率") {
		t.Fatalf("成功率刚好踩到阈值必须拦下: %+v", atSuccess.GateReasons(cfg))
	}
	overSuccess := Evidence{Samples: 1000, OKSamples: 951, SuccessRate: 0.951, ParityTotal: 100, ParityMatch: 100, ParityRate: 1}
	if !overSuccess.Satisfied(cfg) {
		t.Fatalf("成功率刚过阈值必须放行: %+v", overSuccess.GateReasons(cfg))
	}

	// 全绿 = 达标，且原因为空。
	good := Evidence{Samples: 100, OKSamples: 100, SuccessRate: 1, ParityTotal: 100, ParityMatch: 99, ParityRate: 0.99}
	if !good.Satisfied(cfg) || len(good.GateReasons(cfg)) != 0 {
		t.Fatalf("达标的证据不该有拒绝原因: %+v", good.GateReasons(cfg))
	}
}

// TestCutoverRequiresInfra 钉住「不接基础设施就不可能静默成功」：
// 所有改生产流量的写路径在无 DB / 无账号池时必须立刻报错，而不是返回一个看起来
// 成功的零值结果（这类静默成功是切流事故里最难排查的一种）。
func TestCutoverRequiresInfra(t *testing.T) {
	ctx := context.Background()
	svc := NewCutover(nil, CutoverConfig{}, nil)
	if err := svc.EnsureSchema(); err == nil {
		t.Fatal("未接 DB 时 EnsureSchema 必须报错")
	}
	if _, err := svc.Target(ctx, "like", "http://social:8010", "ops", ""); err == nil {
		t.Fatal("未接 DB 时登记切流目标必须报错")
	}
	if _, _, err := svc.Stage(ctx, "like", "v2", 10, "ops"); err == nil {
		t.Fatal("未接 DB 时放量必须报错")
	}
	if _, _, err := svc.Complete(ctx, "like", "ops"); err == nil {
		t.Fatal("未接 DB 时收尾必须报错")
	}
	if err := svc.Rollback(ctx, "like", "一致率跌破阈值", "ops"); err == nil {
		t.Fatal("未接 DB 时回滚必须报错")
	}
	if _, err := svc.Evidence(ctx, "like", "v2"); err == nil {
		t.Fatal("未接 DB 时取证据必须报错")
	}

	ka := NewKeepAlive(nil, nil, nil, nil, KeepAliveConfig{})
	if _, err := ka.Run(ctx); !errors.Is(err, ErrNoAccountProbe) {
		t.Fatalf("未接账号池时保活必须报 ErrNoAccountProbe，得到 %v", err)
	}

	dec := NewDecommission(nil, CutoverConfig{}, 0)
	if dec.stableWindow <= 0 {
		t.Fatal("下线观察窗缺省值异常")
	}
	if _, err := dec.Evaluate(ctx, nil); err == nil {
		t.Fatal("未接 DB 时下线评估必须报错（不能返回一份「可以停服」的计划）")
	}
	if len(DefaultLegacyServices()) != 2 {
		t.Fatal("M4-1 收口的旧服务清单必须是 pycraper + pyreplay 两条")
	}
}

func TestDecommissionPlanText(t *testing.T) {
	pl := Plan{
		Window: (24 * time.Hour).String(),
		Services: []ServicePlan{
			{Service: "pycraper", Skill: "x-scan", Action: ActionStop, Replicas: 0,
				Evidence: Evidence{Samples: 100, OKSamples: 100, SuccessRate: 1, ParityTotal: 100, ParityMatch: 100, ParityRate: 1}},
			{Service: "pyreplay", Skill: "like", Action: ActionKeep, Replicas: 1,
				Blocked: []string{"切流未收尾（status=running traffic=50%）"}},
		},
	}
	pl.Ready = false
	txt := pl.Text()
	for _, want := range []string{"pycraper", "pyreplay", "阻断", "保持现状"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("下线计划文本缺 %q：\n%s", want, txt)
		}
	}
	if !pl.Services[0].Ready() || pl.Services[1].Ready() {
		t.Fatal("Ready 必须只看阻断项")
	}
}

// TestEnsureSchemaIsIdempotent 钉住两处建表的幂等性（MySQL 上是 1050，sqlite 驱动自带
// HasTable 守卫 → 只有真库会炸，必须在这里钉住调用顺序上的「第二次调用」）。
//
// 后果各不相同，所以两条都钉：
//   - CutoverService.EnsureSchema 报错 → cmd/api 的 log.Fatalf，第二次启动直接起不来；
//   - EnsureSocialAccountTables 报错 → cmd/audit 只打一条 WARN，保活巡检被静默关闭。
func TestEnsureSchemaIsIdempotent(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: glogger.Default.LogMode(glogger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewCutover(gdb, CutoverConfig{Actor: "ops"}, nil)
	for i := 0; i < 3; i++ {
		if err := svc.EnsureSchema(); err != nil {
			t.Fatalf("EnsureSchema 第 %d 次必须成功（幂等）: %v", i+1, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := EnsureSocialAccountTables(gdb); err != nil {
			t.Fatalf("EnsureSocialAccountTables 第 %d 次必须成功（幂等）: %v", i+1, err)
		}
	}
}

// TestDropDedupsAlreadyDroppedAccount 钉住「已掉线账号不重复计数」这条约定：
// 长期掉线的账号每个 tick 都会被重新判定一次，dropper 只有在**真正发生状态迁移**时
// 才允许返回 changed=true。
//
// 为什么在 MySQL 上才会暴露：reason 带着随时间增长的秒数（「最近登录已过期（%ds > %ds）」），
// MySQL 默认返回**实际变更行数**，WHERE 只排除 banned 时每个 tick 都会改写 last_error →
// RowsAffected=1 → changed=true，于是同一账号被反复计入 Dropped、反复告警与写审计，
// 还会白吃 MaxDropsPerTick 的预算，把真正需要人工看的掉线挤出本轮。
func TestDropDedupsAlreadyDroppedAccount(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: glogger.Default.LogMode(glogger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := EnsureSocialAccountTables(gdb); err != nil {
		t.Fatalf("ensure social_accounts: %v", err)
	}
	if err := gdb.Exec(
		`INSERT INTO social_accounts (channel, account_id, status, last_error) VALUES ('xhs', 'a1', 'active', '')`).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	drop := DropSocialAccountNeedLogin(gdb)

	changed, dropErr := drop(context.Background(), "xhs", "a1", "最近登录已过期（100s > 60s）")
	if dropErr != nil {
		t.Fatalf("drop: %v", dropErr)
	}
	if !changed {
		t.Fatal("首次掉线必须 changed=true")
	}
	// 同一个 tick 之后 reason 变了（时间推进），状态没变 → 不许再算一次变更。
	changed, dropErr = drop(context.Background(), "xhs", "a1", "最近登录已过期（160s > 60s）")
	if dropErr != nil {
		t.Fatalf("drop again: %v", dropErr)
	}
	if changed {
		t.Fatal("已处于 need_login 的账号必须 changed=false（不重复计数/告警/审计）")
	}
	// banned 是人工终态：巡检既不能计数，也不能把它改回 need_login。
	if banErr := gdb.Exec(`UPDATE social_accounts SET status = 'banned' WHERE account_id = 'a1'`).Error; banErr != nil {
		t.Fatalf("ban: %v", banErr)
	}
	if changed, dropErr = drop(context.Background(), "xhs", "a1", "最近登录已过期（220s > 60s）"); dropErr != nil || changed {
		t.Fatalf("banned 账号必须 changed=false，得到 %v / %v", changed, dropErr)
	}
	var status string
	if err := gdb.Raw(`SELECT status FROM social_accounts WHERE account_id = 'a1'`).Scan(&status).Error; err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "banned" {
		t.Fatalf("巡检不得解除 banned，得到 %q", status)
	}
}

// TestSkillCutoverDDLMatchesGORMColumns 钉住「两条建表路径产出同一张表」：
// hacks/sql/m4_1_cutover.sql（bootstrap.sh，只跑 SQL 的环境）与 CutoverService.EnsureSchema
// （跑 Go 服务的环境，GORM 建表）必须给出**完全相同的列集合**。
//
// 为什么必须逐列比对：字段名 TargetVer / LegacyVer 会被 GORM 推导成 target_ver / legacy_ver，
// 与脚本里的 target_version / legacy_version 不一致。先跑 bootstrap 建表的环境里，
// EnsureSchema 的 HasTable 守卫会直接跳过建表（不会补列），于是 GORM 的每条读写都按
// target_ver 生成 SQL → MySQL 1054 Unknown column，切流 API 全挂，而且只在运行时暴露。
func TestSkillCutoverDDLMatchesGORMColumns(t *testing.T) {
	sqlPath := "../../hacks/sql/m4_1_cutover.sql"
	raw, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("读 %s: %v", sqlPath, err)
	}
	ddl := string(raw)
	start := strings.Index(ddl, "CREATE TABLE IF NOT EXISTS skill_cutover (")
	if start < 0 {
		t.Fatal("DDL 里找不到 CREATE TABLE IF NOT EXISTS skill_cutover")
	}
	end := strings.Index(ddl[start:], "\n)")
	if end < 0 {
		t.Fatal("DDL 的 CREATE TABLE 未闭合")
	}
	ddlCols := map[string]bool{}
	for _, line := range strings.Split(ddl[start:start+end], "\n")[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "PRIMARY", "UNIQUE", "KEY", "INDEX", "CONSTRAINT":
			continue
		}
		ddlCols[strings.Trim(fields[0], "`")] = true
	}

	gormCols := map[string]bool{}
	stmt := &gorm.Statement{DB: gdbForSchemaProbe(t)}
	if err := stmt.Parse(&db.SkillCutover{}); err != nil {
		t.Fatalf("parse model: %v", err)
	}
	for _, f := range stmt.Schema.Fields {
		if f.DBName == "" {
			continue
		}
		gormCols[f.DBName] = true
	}

	var missing, extra []string
	for c := range gormCols {
		if !ddlCols[c] {
			missing = append(missing, c)
		}
	}
	for c := range ddlCols {
		if !gormCols[c] {
			extra = append(extra, c)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		sort.Strings(missing)
		sort.Strings(extra)
		t.Fatalf("skill_cutover 的两条建表路径列集合不一致：GORM 需要而 DDL 缺 = %v；DDL 有而 GORM 不认 = %v",
			missing, extra)
	}
}

// TestSkillCutoverLegacyColumnPatches 钉住老库补列存储过程的形状（评审 bug 的直接回归）：
// 每个补丁列必须**独立**判断存在性、**各自**一条 ALTER。
//
// 反例（曾经的样子）：把 note 的 ADD 挂在 actor 的 NOT EXISTS 分支里 ——
//   - 老库有 note 缺 actor：同一条 ALTER 里 ADD note → 1060 Duplicate column name，
//     整条 ALTER 失败 → CALL 失败 → bootstrap.sh（set -euo pipefail）中断，
//     恰好砸在脚本本来要救的那类老库上；
//   - 老库有 actor 缺 note：note 永远补不上，后续 GORM Save 仍 1054。
//
// 两种坏情况都只在「先跑 bootstrap 的老环境」暴露，Go 侧单测碰不到 mysql CLI，
// 所以这里用结构约束把形状钉死：每个补丁列恰好一次存在性判断 + 一条只含一个
// ADD COLUMN 的 ALTER（逗号接行的多列 ALTER 同样被拒）。
func TestSkillCutoverLegacyColumnPatches(t *testing.T) {
	sqlPath := "../../hacks/sql/m4_1_cutover.sql"
	raw, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("读 %s: %v", sqlPath, err)
	}
	ddl := string(raw)
	proc := ddl[strings.Index(ddl, "CREATE PROCEDURE skill_cutover_migrate_columns"):]

	// 「每个补丁列恰好一次存在性判断」。
	for _, col := range []string{"completed_at", "actor", "note"} {
		want := "AND COLUMN_NAME = '" + col + "')"
		if n := strings.Count(proc, want); n != 1 {
			t.Fatalf("补丁列 %s 的存在性判断出现 %d 次（应为 1 次）：%s", col, n, proc)
		}
	}

	// 「每条 ALTER 只加一列」：ADD COLUMN 行数 == ADD COLUMN 子句数（逗号续行会被算成多子句）。
	addLines := 0
	addClauses := 0
	for _, line := range strings.Split(proc, "\n") {
		if n := strings.Count(line, "ADD COLUMN"); n > 0 {
			addLines++
			addClauses += n
		}
	}
	if addClauses != addLines {
		t.Fatalf("存在一条 ALTER TABLE 用逗号一次 ADD 多列：ADD COLUMN 子句 %d 个 / 行 %d 行。"+
			"补列必须逐列一条 ALTER，否则任一列已存在就整条 1060、bootstrap.sh 中断", addClauses, addLines)
	}
	// 三个补丁列 = 三条 ADD COLUMN。
	if addClauses != 3 {
		t.Fatalf("补丁 ADD COLUMN 共 %d 个（completed_at / actor / note 各一个）：%s", addClauses, proc)
	}
}

// TestCutoverAssemblySurvivesBadEnv 钉住装配层（cmd/api/server.go）对「.env 写坏」的缺省补齐。
//
// LoadEnvConfig 只要任一字段类型不合法就返回 (nil, err) —— 它自己不会带回缺省值，而这份
// EnvConfig 是 keepalive / 切流 / 下线三个组件共用的：一个与切流毫无关系的
// KEEPALIVE_STALE_SEC=3d 也能让 envCfg == nil。装配处若把 stableWindow 声明成零值、只在
// envCfg != nil 时赋值，后面那句「stableWindow <= 0 就 log.Fatalf」的启动自检就会把
// 「env 写错」误报成「切流护栏配置非法」，API 直接起不来（而 NewDecommission 本来就会把
// <=0 归一成 24h）。这里按装配处的同一套写法断言两条路径都能拿到正阈值 + 正窗口。
func TestCutoverAssemblySurvivesBadEnv(t *testing.T) {
	// 与切流无关的字段写坏：envCfg == nil，缺省必须自己补齐。
	t.Setenv("KEEPALIVE_STALE_SEC", "3d")
	envCfg, err := LoadEnvConfig()
	if err == nil {
		t.Fatal("前置条件：类型不合法的 env 必须让 LoadEnvConfig 失败")
	}
	cutoverCfg := CutoverConfig{Actor: "cutover"}.WithDefaults()
	stableWindow := 24 * time.Hour
	if envCfg != nil {
		cutoverCfg = envCfg.CutoverConfig()
		stableWindow = envCfg.DecommissionWindow()
	}
	if cutoverCfg.MinStageInterval <= 0 || cutoverCfg.MinSamples <= 0 ||
		cutoverCfg.ParityMin <= 0 || cutoverCfg.SuccessMin <= 0 || stableWindow <= 0 {
		t.Fatalf("env 解析失败时缺省必须补齐（阈值与观察窗都要 >0）: %+v 观察窗=%s", cutoverCfg, stableWindow)
	}

	// env 正常：阈值/窗口按环境变量生效（缺省补齐不能被写成恒等于缺省）。
	t.Setenv("KEEPALIVE_STALE_SEC", "259200")
	t.Setenv("CUTOVER_MIN_SAMPLES", "100")
	t.Setenv("CUTOVER_SUCCESS_MIN", "0.99")
	t.Setenv("DECOMMISSION_STABLE_SEC", "7200")
	envCfg, err = LoadEnvConfig()
	if err != nil {
		t.Fatalf("LoadEnvConfig: %v", err)
	}
	cutoverCfg = envCfg.CutoverConfig()
	stableWindow = envCfg.DecommissionWindow()
	if cutoverCfg.MinSamples != 100 || cutoverCfg.SuccessMin != 0.99 || stableWindow != 2*time.Hour {
		t.Fatalf("环境变量没生效: %+v 观察窗=%s", cutoverCfg, stableWindow)
	}
}

// gdbForSchemaProbe 给列名比对用的 GORM 会话（sqlite 建表后立刻解析模型，不写数据）。
func gdbForSchemaProbe(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: glogger.Default.LogMode(glogger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return gdb
}
