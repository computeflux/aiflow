// cutover.go：skill 网关的「跨服务切流」（M4-1）。
//
// 与 S1 的灰度（approval.TickGray：新版本上线，5→50→100）不同，本文件做的是
// **同源能力的跨服务迁移**：x-scan / like 的能力从 pycraper / pyreplay 收口到 social，
// 两版实现的输入/输出契约相同（口径由 skillcontract 与 apps/social 的测试钉住），
// 差别只在 addr（新 addr = http://social:8010）。于是切流就是：在同一个 skill 上登记
// 一条指向 social 的版本行（skill_versions.config.addr），并按 10% → 50% → 100% 放量。
//
// 每一档的升档依据是两件事（对应验收标准里的「一致率与成功率」）：
//   - 成功率：窗口内该版本生产调用（skill_runs）里 status=ok 的占比 ≥ SuccessMin；
//   - 一致率：同一批入参在新旧两版各跑一次（对比探针，skill_runs.workflow_id = 'parity:<组>'）
//     里结果摘要一致的组占比 ≥ ParityMin。
//     **零证据不放量**：样本数不足 MinSamples 一律判不达标（与 S1 §3.4「100% 档要真实
//     流量证据」同口径 —— 没有证据的「零失败」什么都证明不了）。
//
// skills.addr 是基线服务地址（S5 的 deploy.VersionManager.baseService 靠它找回 compose
// 服务），所以切流过程中**绝不**改它，只在 Complete（全量收尾）时收敛到目标 addr；
// 全程可用 Rollback 撤销（旧版本恢复 100%，skills.addr 不动）。
package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// CutoverStages 是切流档位序列（10% → 50% → 100%）。
var CutoverStages = []int{10, 50, 100}

// ParityPrefix 是对比探针的 skill_runs.workflow_id 前缀。
// 探针由运维/巡检用同一批入参分别打新旧两版生成（RecordParityRun），与生产 workflow
// 分开记，避免污染 S1 灰度窗口的统计口径（TickGray 也读 skill_runs）。
const ParityPrefix = "parity:"

// 切流状态（skill_cutover.status）：取值定义在 db 包（表所属层），这里只做别名以保留
// migration 包内的既有写法。跨包读这张表的守卫（approval 的 TickGray 让路判定）必须引用
// db.CutoverStatus* / db.CutoverStatusActive，两侧共用一份取值才不会一边改名一边静默失效。
const (
	CutoverPending    = db.CutoverStatusPending
	CutoverRunning    = db.CutoverStatusRunning
	CutoverCompleted  = db.CutoverStatusCompleted
	CutoverRolledBack = db.CutoverStatusRolledBack
)

// 切流常见错误。
var (
	ErrCutoverStage     = errors.New("migration: 非法切流档位")
	ErrCutoverInterval  = errors.New("migration: 距上一档不足最短观察间隔，禁止放量")
	ErrCutoverNoTarget  = errors.New("migration: 该 skill 没有切流中的版本")
	ErrCutoverNotParsed = errors.New("migration: 该 skill 未登记切流目标")
	ErrCutoverCompleted = errors.New("migration: 该 skill 的切流已收尾，不许再改流量")
	// ErrCutoverVersionLocked：想把流量切到「本切流没登记过的现存版本」（典型是 live 基线）。
	ErrCutoverVersionLocked = errors.New("migration: 该版本号不是本切流的目标版本，禁止改写")
)

// CutoverConfig 是切流的缺省护栏（skill_cutover 行上的阈值为 0 时回落到这里）。
type CutoverConfig struct {
	// Actor 是缺省审计操作者。
	Actor string
	// MinStageInterval 是相邻两档的最短观察间隔。
	MinStageInterval time.Duration
	// MinSamples 是每档要求的最少样本数（成功率与一致率共用）。
	MinSamples int
	// ParityMin / SuccessMin 是缺省阈值（0~1）。
	ParityMin  float64
	SuccessMin float64
}

// WithDefaults 填充缺省：相邻档位最短观察 30 分钟、每档 ≥20 个样本、
// 一致率 ≥90%、成功率 ≥95%。
func (c CutoverConfig) WithDefaults() CutoverConfig {
	if c.Actor == "" {
		c.Actor = "cutover"
	}
	if c.MinStageInterval <= 0 {
		c.MinStageInterval = 30 * time.Minute
	}
	if c.MinSamples <= 0 {
		c.MinSamples = 20
	}
	if c.ParityMin <= 0 {
		c.ParityMin = 0.9
	}
	if c.SuccessMin <= 0 {
		c.SuccessMin = 0.95
	}
	return c
}

// effective 用切流行上的阈值覆盖缺省（0 = 未设置，沿用缺省）。
func (c CutoverConfig) effective(row *db.SkillCutover) CutoverConfig {
	out := c.WithDefaults()
	if row == nil {
		return out
	}
	if row.ParityMin > 0 {
		out.ParityMin = row.ParityMin
	}
	if row.SuccessMin > 0 {
		out.SuccessMin = row.SuccessMin
	}
	if row.MinSamples > 0 {
		out.MinSamples = row.MinSamples
	}
	return out
}

// CutoverService 执行切流：登记目标 → 逐档放量 → 达标收尾 → 恶化回滚。
type CutoverService struct {
	db  *gorm.DB
	cfg CutoverConfig

	// probePrefix 参与「最新一批探针」的识别（见 probeBatchSeqOf）。
	probePrefix string

	// auditFn / versionLookup 是可选的注入点（仅测试用；生产为 nil = 走 DB）。
	auditFn func(ctx context.Context, actor, action, target string, detail any) error

	// legacyLookup 覆盖「取除切流版本外最早版本行」的兜底查询（仅测试用：生产为 nil = 走 DB；
	// sqlite 不支持生产 SQL 里的 `skill_id = (SELECT ...)` 关联子查询写法，需按等价语义注入）。
	legacyLookup func(ctx context.Context, skillName, excludeVersion string) (*db.SkillVersion, error)
	// anyVersionLookup 覆盖「按 skill 名 + 版本号取任意 status 的版本行」（仅测试用，理由同上）。
	anyVersionLookup func(ctx context.Context, skillName, version string) (*db.SkillVersion, error)

	// versionLookup 覆盖「按 skill 名 + 版本号取版本行」的查询（仅测试用：sqlite 不支持
	// 生产 SQL 里的 `skill_id = (SELECT id FROM skills WHERE name = ?)` 关联子查询写法）。
	versionLookup func(ctx context.Context, skillName, version string) (*db.SkillVersion, error)

	alert AlertFunc
	now   func() time.Time
}

// NewCutover 构造切流服务（alert 为 nil = 不发告警）。
func NewCutover(gdb *gorm.DB, cfg CutoverConfig, alert AlertFunc) *CutoverService {
	if alert == nil {
		alert = func(context.Context, string, string, string) error { return nil }
	}
	return &CutoverService{db: gdb, cfg: cfg.WithDefaults(), probePrefix: ParityPrefix, alert: alert, now: time.Now}
}

// Config 返回生效中的护栏配置（供下游组件复用同一套阈值，例如 decommission 的证据判定）。
func (s *CutoverService) Config() CutoverConfig { return s.cfg }

// EnsureSchema 建 skill_cutover（幂等：表已存在直接返回 nil）。
//
// 为什么必须先 HasTable：Migrator().CreateTable 下发的是**普通 CREATE TABLE**（不判存在），
// MySQL 上表已存在会返回 1050。而调用方（cmd/api/server.go）对它的错误是 log.Fatalf ——
// 于是「第二次启动」与「先跑过 hacks/sql/m4_1_cutover.sql 的环境」都会让 API 起不来。
// sqlite 驱动内部有 HasTable 守卫，单测看不出这个差异，必须在这里显式挡。
// 仍用 GORM 建表而不是裸 DDL：同一份模型定义同时服务 MySQL 与单测的 sqlite :memory:
// （DDL 里的 ENGINE / ON UPDATE / DATETIME(3) 方言在 sqlite 上直接语法报错）。
func (s *CutoverService) EnsureSchema() error {
	if s.db == nil {
		return ErrNoDB
	}
	if s.db.Migrator().HasTable(&db.SkillCutover{}) {
		return nil
	}
	return s.db.Migrator().CreateTable(&db.SkillCutover{})
}

// Target 登记（或更新）某 skill 的切流目标。已放量到中间档时禁止改目标 addr。
func (s *CutoverService) Target(ctx context.Context, skillName, targetAddr, actor, note string) (*db.SkillCutover, error) {
	if s.db == nil {
		return nil, ErrNoDB
	}
	targetAddr = strings.TrimSuffix(strings.TrimSpace(targetAddr), "/")
	if targetAddr == "" {
		return nil, errors.New("target_addr 不能为空")
	}
	row, err := s.get(ctx, skillName)
	if err != nil {
		return nil, err
	}
	// 只要放过量（traffic > 0，含已全量），就禁止换目标 addr：已全量意味着 skills.addr
	// 已收敛到旧目标，此时换目标会让「基线服务」与「切流目标」指向两个不同实现，
	// 回滚路径直接失效。要换目标必须先回滚到 0%。
	if row != nil && row.Traffic > 0 && row.TargetAddr != targetAddr {
		return nil, fmt.Errorf("切流已在 %d%% 档，禁止更换目标 addr（先回滚到 0%%）", row.Traffic)
	}
	if actor == "" {
		actor = s.cfg.Actor
	}
	if row == nil {
		row = &db.SkillCutover{
			SkillName:  skillName,
			TargetAddr: targetAddr,
			Status:     CutoverPending,
			Actor:      actor,
			Note:       note,
			CreatedAt:  s.now(),
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			return nil, fmt.Errorf("create skill_cutover: %w", err)
		}
	} else {
		row.TargetAddr = targetAddr
		row.Actor = actor
		if note != "" {
			row.Note = note
		}
		row.UpdatedAt = s.now()
		if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
			return nil, fmt.Errorf("update skill_cutover: %w", err)
		}
	}
	s.log(ctx, "cutover_target", skillName, map[string]any{"target_addr": targetAddr, "actor": actor}, actor)
	return row, nil
}

// Get 返回某 skill 的切流状态（不存在返回 nil, nil）。
func (s *CutoverService) Get(ctx context.Context, skillName string) (*db.SkillCutover, error) {
	return s.get(ctx, skillName)
}

// List 返回全部切流状态（新记录在前）。
func (s *CutoverService) List(ctx context.Context) ([]db.SkillCutover, error) {
	if s.db == nil {
		return nil, ErrNoDB
	}
	var rows []db.SkillCutover
	if err := s.db.WithContext(ctx).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list skill_cutover: %w", err)
	}
	return rows, nil
}

// Stage 把某 skill 的切流放量到指定档位（10 / 50 / 100）。
//
// 护栏顺序：档位合法 → 已登记目标 → 质量护栏 → 时长护栏 → 才写流量。
// 任一护栏不过返回错误且**不写任何流量**（调用方把原因回给操作者）。
//
// 质量护栏对「首次上灰度」与「升档」区别对待，这是唯一能让闭环转起来的口径：
//   - 首次上灰度（该版本此前从未带流量）：证据窗口内不可能有数据，硬卡样本量会让
//     第一档永远切不上去（先有鸡还是先有蛋）。因此只要求「对比探针全部跑完且一致」——
//     离线双跑（同一批入参新旧各跑一次）是上灰度的门槛，零探针 = 拒绝；
//   - 升档 / 全量：必须看过真实流量证据（样本量 + 一致率 + 成功率全部达标）。
func (s *CutoverService) Stage(ctx context.Context, skillName, version string, traffic int, actor string) (*db.SkillVersion, *Evidence, error) {
	if s.db == nil {
		return nil, nil, ErrNoDB
	}
	if !ValidStage(traffic) {
		return nil, nil, fmt.Errorf("%w: %d（合法档位 %s）", ErrCutoverStage, traffic, StagesLabel())
	}
	row, err := s.get(ctx, skillName)
	if err != nil {
		return nil, nil, err
	}
	if row == nil {
		return nil, nil, fmt.Errorf("%w: %s（先登记 target_addr）", ErrCutoverNotParsed, skillName)
	}
	// 已收尾 = skills.addr 已收敛到新实现、其余版本全部 retired/0。此时再改档位：
	// applyTraffic 找不到任何承接剩余流量的旧版本（others 为空），切流版本被写成 gray+10%
	// 后就是路由池里的唯一一行 —— 网关按各行流量归一加权，线上仍是 100% 打新实现，
	// 而 skill_cutover 记着「10%」：状态机与真实流量彻底脱节。随后的 Complete 又会因
	// traffic < 100 被拒，只能逐档放量绕回来。要重切必须先回滚（把基线 addr 一并改回去）。
	if row.Status == CutoverCompleted {
		return nil, nil, fmt.Errorf("%w: %s（收尾后 skills.addr 已收敛到 %s，禁止再改流量；要重切请先回滚）",
			ErrCutoverCompleted, skillName, row.TargetAddr)
	}
	cfg := s.cfg.effective(row)

	var skill db.Skill
	if err := s.db.WithContext(ctx).Model(&db.Skill{}).Where("name = ?", skillName).First(&skill).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, fmt.Errorf("skill %s 不存在", skillName)
		}
		return nil, nil, fmt.Errorf("query skill: %w", err)
	}

	v, created, err := s.ensureTargetVersion(ctx, &skill, row, version)
	if err != nil {
		return v, nil, err
	}
	// 「首档」= 该版本当前没有生产流量在跑，质量依据只能是离线双跑探针（FirstStageReasons）。
	// 四种情形都算首档：
	//   - created：本次刚建的版本行；
	//   - row.TargetVer == ""：只登记了 target、从未放过量；
	//   - row.Status == rolled_back：回滚已把目标版本置 retired/traffic_split=0，
	//     此后不可能再产生该版本的生产样本。若这里按「升档」走 GateReasons（要求 stage_at
	//     之后 ≥MinSamples 条生产样本），样本恒为 0 → 永远「成功率样本不足」，
	//     「回滚 → 修复 → 重切」这条链在生产上被永久锁死。
	//
	//   - row.Traffic == 0：同理（零流量 = 没有样本来源）。
	//
	// 与 approval.TickGray 的联动（那边用 db.CutoverStatusActive 判「切流是否活跃」）：rolled_back
	// 在这里算首档 = 重新放量时 Stage 会先把 status 写成 running（见下面「先存切流行再写流量」
	// 的顺序保证），所以回滚后的同版本重新放量一定会重新被切流接管，灰度推进不会漏掉它 ——
	// 两侧对「终态让路、重放重新接管」的口径必须一致。
	// 注意不能用「本次刚 INSERT 版本行」判定：护栏拒绝时版本行已经落库（保证可重试），
	// 若按 created 判定，重试会把首档当成「升档」去要生产流量样本 → 首档永远切不上去。
	isNew := created || row.TargetVer == "" || row.Status == CutoverRolledBack || row.Traffic == 0

	// 时长护栏：同一档重复执行（幂等）放行；首档（该版本此前从未带流量）放行；
	// 只有「升档」必须看够一个观察窗口。
	// 幂等判定同时看 row.Traffic 与版本行的 traffic_split：前者在写流量之前就已落库
	// （见下面「先存切流行再写流量」的顺序注释），后者由 applyTraffic 写。两处任一命中都算
	// 「这一档已经放过」—— 覆盖「切流行写成功、版本流量写失败」的半程失败，让运维重放同档即可。
	isSameStage := !isNew && (v.TrafficSplit == traffic || row.Traffic == traffic) && row.TargetVer == v.Version
	if !isSameStage && row.StageAt != nil {
		if elapsed := s.now().Sub(*row.StageAt); elapsed < cfg.MinStageInterval {
			return v, nil, fmt.Errorf("%w: 已观察 %s，需要 %s", ErrCutoverInterval,
				elapsed.Truncate(time.Second), cfg.MinStageInterval)
		}
	}

	// 质量护栏（放在时长护栏之后：被时长拒绝的调用不该把探针当证据消费掉）。
	ev, err := s.Evidence(ctx, skillName, v.Version)
	if err != nil {
		return v, nil, err
	}
	// ⚠️ 首档豁免只对**第一档**生效。isNew 判的是「该版本当前没有生产流量」，不是「本次请求的
	// 是首档」：不钉住档位，一次 POST /skill-cutover/:skill/stage 传 traffic_split=100 就能让
	// 新版本走首档分支 —— 只看离线双跑、MinSamples/SuccessMin 全部跳过（且新建版本时 row.StageAt
	// 为 nil，连时长护栏都不参与），一次调用即可把生产流量全量打到从未跑过真实流量的新实现。
	// Complete 之后的 GateReasons 只拦「收尾」这一步，那时流量已经是 100%，与「10→50→100 逐档
	// 放量、升档必须有真实流量证据」的口径相反。要放量到 50/100 必须先把 10% 跑进观察窗。
	if isNew && traffic != CutoverStages[0] {
		return v, ev, fmt.Errorf("%w: 新版本必须从 %d%% 起放量（当前请求 %d%%），逐档 %s",
			ErrCutoverStage, CutoverStages[0], traffic, StagesLabel())
	}
	if isNew {
		// 首档：生产流量样本还不存在，硬卡样本量会让第一档永远切不上去（先有鸡还是先有蛋）。
		// 门槛换成「离线双跑全部一致」—— 同一批入参新旧各跑一次，逐组比对结果摘要。
		if reasons := ev.FirstStageReasons(); len(reasons) > 0 {
			return v, ev, fmt.Errorf("离线双跑不达标，禁止上灰度到 %d%%：%s", traffic, strings.Join(reasons, "；"))
		}
	} else if reasons := ev.GateReasons(cfg); len(reasons) > 0 {
		return v, ev, fmt.Errorf("观察数据不达标，禁止放量到 %d%%：%s", traffic, strings.Join(reasons, "；"))
	}

	// ⚠️ 顺序是安全属性，不能调换：**先**把 skill_cutover 落库（target_version + status + 档位），
	// **再**写版本流量。applyTraffic 一执行，版本行就是 status=gray + traffic_split>0 —— 对网关
	// 路由与 approval.TickGray 都立即可见；而 TickGray 的归属判定读的是 skill_cutover.target_version
	// （`<> ''` 过滤）。若先写流量、后存切流行，中间任何一步失败都会留下「流量已生效 + target_version
	// 为空」的窗口：TickGray 把这行当普通 S1 灰度接管，advanceGray 的 default 分支（gray_stage=10）
	// 直接置 live/100 并 retire 基线 —— 正是本包要防的「绕过人工护栏全量」。
	// 反过来的最坏情况是「切流行记了档位、版本流量还没写」：路由不动，TickGray 已让路，
	// 运维重放同档即可自愈（幂等判定见 isSameStage）。
	now := s.now()
	row.Traffic = traffic
	row.Status = CutoverRunning
	row.StageAt = &now
	row.TargetVer = v.Version
	if actor == "" {
		actor = s.cfg.Actor
	}
	row.Actor = actor
	row.UpdatedAt = now
	if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
		return v, ev, fmt.Errorf("update skill_cutover: %w", err)
	}

	legacy, err := s.applyTraffic(ctx, skill.ID, v, traffic)
	if err != nil {
		return v, ev, err
	}
	if legacy != "" && legacy != row.LegacyVer {
		row.LegacyVer = legacy
		if err := s.db.WithContext(ctx).Model(&db.SkillCutover{}).Where("id = ?", row.ID).
			Update("legacy_version", legacy).Error; err != nil {
			return v, ev, fmt.Errorf("update skill_cutover legacy version: %w", err)
		}
	}
	s.log(ctx, "cutover_stage", fmt.Sprintf("%s v%s", skillName, v.Version), map[string]any{
		"traffic": traffic, "target_addr": row.TargetAddr, "legacy_version": row.LegacyVer,
		"success_rate": ev.SuccessRate, "parity_rate": ev.ParityRate,
	}, actor)
	return v, ev, nil
}

// Complete 收尾：切流版本转 live/100，其余版本 retired/0，skills.addr 收敛到目标 addr。
func (s *CutoverService) Complete(ctx context.Context, skillName, actor string) (*db.SkillVersion, *Evidence, error) {
	if s.db == nil {
		return nil, nil, ErrNoDB
	}
	row, err := s.get(ctx, skillName)
	if err != nil {
		return nil, nil, err
	}
	if row == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrCutoverNotParsed, skillName)
	}
	// 收尾态检查必须在「档位检查」之前：回滚会把 traffic 归零并把 status 置 rolled_back，
	// 此时先查档位会报「当前 0%，先放量到 100%」——那是对着已回滚的切流说「先去放量」，
	// 把人往「重新放量再收尾」的方向带，正是刚被证明有问题的实现。
	if row.Status == CutoverCompleted {
		return nil, nil, fmt.Errorf("%w: %s（收尾会改 skills.addr 基线并 retire 旧版本，重复执行会把基线越改越偏；要重切请先回滚）",
			ErrCutoverCompleted, skillName)
	}
	if row.Status == CutoverRolledBack {
		return nil, nil, fmt.Errorf("%w: %s 已回滚（rolled_back），禁止收尾（要重切请重新逐档放量）",
			ErrCutoverNoTarget, skillName)
	}
	if row.Traffic < 100 {
		return nil, nil, fmt.Errorf("%w: %s 当前 %d%%，先放量到 100%%", ErrCutoverNoTarget, skillName, row.Traffic)
	}
	cfg := s.cfg.effective(row)

	v, err := s.lookupVersion(ctx, skillName, row.TargetVer)
	if err != nil {
		return nil, nil, err
	}

	ev, err := s.Evidence(ctx, skillName, v.Version)
	if err != nil {
		return v, nil, err
	}
	if reasons := ev.GateReasons(cfg); len(reasons) > 0 {
		return v, ev, fmt.Errorf("观察数据不达标，禁止收尾：%s", strings.Join(reasons, "；"))
	}

	now := s.now()
	v.Status = "live"
	v.TrafficSplit = 100
	v.GrayStage = nil
	v.GrayStageStartedAt = nil
	if err := s.db.WithContext(ctx).Save(v).Error; err != nil {
		return v, ev, fmt.Errorf("promote cutover version: %w", err)
	}
	if err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
		Where("skill_id = ? AND id <> ? AND traffic_split > 0", v.SkillID, v.ID).
		Updates(map[string]any{"traffic_split": 0, "status": "retired"}).Error; err != nil {
		return v, ev, fmt.Errorf("retire legacy versions: %w", err)
	}
	// 基线 addr 收敛（此后 deployctl / S5 版本容器都以新实现为基线）。
	if err := s.db.WithContext(ctx).Model(&db.Skill{}).Where("name = ?", skillName).
		Updates(map[string]any{"addr": row.TargetAddr, "updated_at": now}).Error; err != nil {
		return v, ev, fmt.Errorf("set skills.addr: %w", err)
	}

	row.Status = CutoverCompleted
	// CompletedAt 是「稳定观察窗」的起点（旧服务下线判定用它，不用 updated_at：
	// 收尾之后任何一次改 note/actor 的写操作都会把 updated_at 推后，窗口会被无限延长）。
	row.CompletedAt = &now
	row.UpdatedAt = now
	if actor == "" {
		actor = s.cfg.Actor
	}
	row.Actor = actor
	if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
		return v, ev, fmt.Errorf("update skill_cutover: %w", err)
	}
	s.log(ctx, "cutover_complete", fmt.Sprintf("%s v%s", skillName, v.Version), map[string]any{
		"addr": row.TargetAddr, "success_rate": ev.SuccessRate, "parity_rate": ev.ParityRate,
	}, actor)
	return v, ev, nil
}

// Rollback 恶化回滚：切流版本 traffic=0 且 retired，旧版本恢复 live/100%。
//
// skills.addr 的处理分两种情况，都必须与「线上真正承接流量的实现」一致：
//   - 未收尾（基线 addr 还是旧实现）：不动 skills.addr（这是绝大多数回滚，回滚后行为零变化）；
//   - 已收尾（Complete 已把基线 addr 收敛到目标 addr）：必须把基线 addr 改回**恢复上线的那个
//     旧版本自己 config 里的 addr**。基线 addr 是 S5 deploy.VersionManager.baseService 的来源
//     （internal/deploy/version.go：起版本容器 / 还原 config.addr 都读它），留着 social 的 addr
//     就意味着「回滚窗口里任何一次 deployctl version up」都按新实现的 compose 服务与环境起容器，
//     与线上实际承接流量的旧实现错位。同时清掉 completed_at（稳定观察窗随收尾一起作废）。
func (s *CutoverService) Rollback(ctx context.Context, skillName, reason, actor string) error {
	if s.db == nil {
		return ErrNoDB
	}
	row, err := s.get(ctx, skillName)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("%w: %s", ErrCutoverNotParsed, skillName)
	}
	// ⚠️ 安全属性：demote（摘掉切流版本流量）与 restore（把旧版本拉回 live/100）必须成对生效，
	// 绝不允许出现「旧的已摘、新的没接」的中间态 —— 那时库里没有任何
	// `status IN (live,gray) AND traffic_split > 0` 的行，skillgateway.Registry.ResolveLive 直接
	// ErrNoLiveVersion，该 skill 线上不可服务；而切流行一旦被本函数置成 rolled_back，Complete 被禁、
	// Stage 首档要重过观察窗，状态机不会自愈，只能人工救。
	//
	// 落地方式：先查清「谁来接替」（legacy_version 精确查 → 兜底查），demote 之后若发现无人接替
	// （old == nil）而切流版本本来正承接流量（servedTraffic > 0），就把它的流量**原样恢复**并让
	// 本次回滚整体失败。demote 与恢复之间没有对外可见的窗口（同一个 sqlite/MySQL 连接上的紧邻两条
	// UPDATE；跨请求的最坏情况是本函数返回错误、运维收到告警后重试），而「不恢复」的后果是持续的
	// 不可服务 —— 两者取其一，必须选前者。
	var skill db.Skill
	if err := s.db.WithContext(ctx).Model(&db.Skill{}).Where("name = ?", skillName).First(&skill).Error; err != nil {
		return fmt.Errorf("query skill: %w", err)
	}
	cur, err := s.lookupVersionOptional(ctx, skillName, row.TargetVer)
	if err != nil {
		return fmt.Errorf("query target version: %w", err)
	}
	// servedTraffic = 切流版本在**本次回滚之前**承接的生产流量档位。必须在 demote 之前取：
	// 下面会把它的 traffic_split 归零，用归零后的值判「有没有流量要撤」会让
	// 「已全量 + 无接替版本」那条路被当成无事发生直接放行（正是本函数要拦的故障）。
	// demote 之前的快照：流量档位 + status + 灰度档位。三样都要，撤销 demote 时整套写回。
	servedTraffic, preStatus := 0, ""
	var preGrayStage *int
	var preGrayStageStartedAt *time.Time
	curID := uint(0) // 0 = 没有切流版本行；liveTrafficOf 的 id <> 条件对任何真实行都恒真
	if cur != nil {
		servedTraffic = cur.TrafficSplit
		preStatus = cur.Status
		preGrayStage = cur.GrayStage
		preGrayStageStartedAt = cur.GrayStageStartedAt
		curID = cur.ID
	}

	// 恢复对象 = skill_cutover.legacy_version（切流时 applyTraffic 记下的「真正在承接流量的
	// 旧版本」），只有它查不到时才退回「除切流版本外最早的一行」兜底。
	//
	// 为什么必须优先精确查：S1 灰度会不断累积版本行（5→50→100 每轮都留行），
	// lookupLegacyVersion 按 id ASC 取的是**最老**的那一行 —— 直接用它，回滚会把生产流量
	// 切到很久以前的实现（可能是几个版本前、早已废弃的镜像），而不是刚刚被切下来的那一版。
	restored := row.LegacyVer
	var old *db.SkillVersion
	if restored != "" {
		// 用 lookupAnyVersion 而不是 lookupVersionOptional：收尾（Complete）会把其余版本行
		// 写成 retired + traffic_split=0，而 legacy_version 记的正是那一行。收尾后的回滚要恢复
		// 的就是它 —— 按 status 过滤会让这条精确恢复路径永远落空，白白退到「取最老一行」的兜底。
		if old, err = s.lookupAnyVersion(ctx, skillName, restored); err != nil {
			return fmt.Errorf("query legacy version: %w", err)
		}
	}
	// 兜底查询只在「本切流确实放过量」（TargetVer 非空）时才允许：那时 legacy_version 缺失
	// 属于历史数据缺口，退而求其次找「除切流版本外最早的版本行」总比没有强。
	//
	// 「只登记了 target、从未放量」时绝不能兜底：那种回滚里 LegacyVer 与 TargetVer 都是空，
	// 兜底查询按 id ASC 取到的是**最老**的一行 —— 多版本 skill（S1 灰度每轮都留行）会把几个
	// 版本前早已废弃的实现重新拉成 live/100%，与当前 live 基线各带 100% 流量（网关按流量归一
	// ≈ 一半真实流量打到废弃实现）；而切流行已被置 rolled_back，Complete 被禁止，状态机不会自愈。
	// 从未放量 = 线上没有任何东西需要撤销，只更新切流行状态即可。
	if old == nil && row.TargetVer != "" {
		if old, err = s.lookupLegacyVersion(ctx, skillName, row.TargetVer); err != nil {
			return fmt.Errorf("query legacy version: %w", err)
		}
	}
	// 一个可接替版本都没找到，而切流版本**刚刚还在承接生产流量**（servedTraffic>0，含收尾后
	// live/100）：demote 之后路由池里 `status IN (live,gray) AND traffic_split > 0` 的行数恰好
	// 等于本次回滚前它自己承接的档位数（收尾后那一版 = 1，中间档 = 2，applyTraffic 保证其余
	// 版本按现有流量等比承接剩下的 100-traffic）。这里把它原样恢复回去并中止回滚，最坏只是
	// 「回滚没做成」，绝不会留下 ResolveLive 无 live 可用的空路由池（那会让该 skill 调用全挂，
	// 且切流行已 rolled_back、Complete 被禁，状态机不会自愈）。
	if cur != nil {
		cur.TrafficSplit = 0
		cur.Status = "retired"
		cur.GrayStage = nil
		cur.GrayStageStartedAt = nil
		if err := s.db.WithContext(ctx).Save(cur).Error; err != nil {
			return fmt.Errorf("demote cutover version: %w", err)
		}
		// 显式下发 traffic_split：该列带 gorm `default:100`，Save 会跳过零值 → 库里留着原档位，
		// 回滚后的新实现仍在路由池里（ResolveLive 只看库里的 traffic_split > 0）。
		if err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
			Where("id = ?", cur.ID).Update("traffic_split", 0).Error; err != nil {
			return fmt.Errorf("demote cutover traffic: %w", err)
		}
	}
	// demote 之后、恢复之前算一次：liveLeft = 摘掉切流版本之后路由池里剩下的流量之和。
	// 与 servedTraffic 比较而不是与 0 比较 —— 「收尾后的回滚」里其余版本已被 Complete 置成
	// retired/0，liveLeft 恒为 0，只有和「摘掉了多少」比才能区分「有人接替」与「摘完就空」。
	liveLeft, err := s.liveTrafficOf(ctx, skill.ID, curID)
	if err != nil {
		return err
	}
	if old != nil {
		old.TrafficSplit = 100
		old.Status = "live"
		if err := s.db.WithContext(ctx).Save(old).Error; err != nil {
			return fmt.Errorf("restore legacy version: %w", err)
		}
		// 同 demote：显式下发这一列，保证「恢复到 100%」真的落到库里（回滚时旧实现可能已被
		// 承接为 90/50，这里必须精确写回 100，不能依赖 Save 的字段过滤行为）。
		if err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
			Where("id = ?", old.ID).Update("traffic_split", 100).Error; err != nil {
			return fmt.Errorf("restore legacy traffic: %w", err)
		}
		restored = old.Version
	}

	// demote 摘掉切流版本之后，路由池里剩下的流量（liveLeft）**必须补得上摘掉的量**（servedTraffic）。
	// 补不上 = 「旧的已摘、新的没接」：网关 ResolveLive 按各行 traffic_split 归一加权，剩下的量
	// 不足（极端情形全零 = ErrNoLiveVersion）就意味着该 skill 线上不可服务；而本函数一旦把切流行
	// 置成 rolled_back，Complete 被禁、Stage 首档要重过观察窗，状态机不会自愈，只能人工救。
	//
	// 与「行数」比不行，必须与「摘掉了多少」比：收尾后的回滚里其余版本已被 Complete 置成 retired/0，
	// liveLeft 恒为 0；中间档回滚时 applyTraffic 让其余带流量版本等比承接剩下的 100-traffic，
	// liveLeft = 100-servedTraffic，正常路径下 liveLeft + servedTraffic = 100。
	//
	// 处置：把切流版本的流量与 status 一起原样恢复（demote 撤销）并让本次回滚整体失败。最坏只是
	// 「回滚没做成」—— 切流行还停在原状态，运维补好旧实现版本行后原样重试即可。
	if old == nil && servedTraffic > 0 && liveLeft < servedTraffic {
		// 撤销 demote 要连 status / gray_stage 一起回滚成 demote **之前**的值（preStatus /
		// preGrayStage / preGrayStageStartedAt，都在改动内存对象之前取好）。只补 traffic_split
		// 会留下 `status=retired AND traffic_split=100` —— ResolveLive 只认
		// status IN (live,gray) AND traffic_split > 0，那等于什么都没恢复。
		if err := s.db.WithContext(ctx).Unscoped().Model(&db.SkillVersion{}).
			Where("id = ?", cur.ID).
			Updates(map[string]any{
				"traffic_split":         servedTraffic,
				"status":                preStatus,
				"gray_stage":            preGrayStage,
				"gray_stage_started_at": preGrayStageStartedAt,
			}).Error; err != nil {
			return fmt.Errorf("restore cutover traffic after aborted rollback: %w", err)
		}
		return fmt.Errorf(
			"回滚中止：%s 没有可接替的旧版本（legacy_version=%q 与兜底查询都没查到版本行），"+
				"已把 v%s 原样恢复回 %d%% / %s（该 skill 线上未受影响）；"+
				"请先确认旧实现版本行还在（skill_versions），再重试回滚",
			skillName, row.LegacyVer, cur.Version, servedTraffic, preStatus)
	}

	// 收尾后回滚：基线 addr 跟着流量一起回退（见函数注释）。
	// 判据用「当前基线 addr == 切流目标 addr」：未收尾的回滚里基线本来就是旧实现 addr，不会命中；
	// 命中说明确实是 Complete 改写过。回写值取恢复上线那一版自己 config 里的 addr（建行时从基线
	// 继承来的旧实现地址），不需要给 skill_cutover 再加一列历史 addr。
	// old == nil（没有旧版本行可恢复）时绝不回写：基线必须继续指向新实现 —— 那时流量本来就全在
	// 新实现上，把基线改回一个不存在的旧地址会让 S5 起容器起空。
	if row.Status == CutoverCompleted && row.TargetAddr != "" && old != nil {
		var skill db.Skill
		if err := s.db.WithContext(ctx).Where("name = ?", skillName).First(&skill).Error; err != nil {
			return fmt.Errorf("query skill: %w", err)
		}
		if skill.Addr == row.TargetAddr {
			baseAddr := configAddrOf(old.Config)
			if baseAddr == "" {
				return fmt.Errorf("回滚无法还原 skills.addr：%s v%s 的 config 里没有 addr（基线地址停在切流目标 %s，请人工核对后改回）",
					skillName, restored, row.TargetAddr)
			}
			if err := s.db.WithContext(ctx).Model(&db.Skill{}).Where("name = ?", skillName).
				Updates(map[string]any{"addr": baseAddr, "updated_at": s.now()}).Error; err != nil {
				return fmt.Errorf("restore skills.addr: %w", err)
			}
			row.CompletedAt = nil // 收尾已撤销：旧服务下线的稳定观察窗随之作废
		}
	}

	now := s.now()
	row.Traffic = 0
	row.Status = CutoverRolledBack
	// 回滚后必须重新观察满一个窗口才能再次上第一档：Stage 的时长护栏读 stage_at，且只在
	// 「row.StageAt != nil」时判间隔 —— 留空等于整条时间护栏跳过，刚回滚完就能立刻把流量再推
	// 上去。起点取回滚时刻，让「回滚 → 修复 → 重切」之间天然隔出一个观察期（与升档同口径）。
	row.StageAt = &now
	row.UpdatedAt = now
	if actor == "" {
		actor = s.cfg.Actor
	}
	row.Actor = actor
	row.Note = reason
	if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
		return fmt.Errorf("update skill_cutover: %w", err)
	}
	s.log(ctx, "cutover_rollback", fmt.Sprintf("%s v%s", skillName, row.TargetVer), map[string]any{
		"reason": reason, "rolled_back_to": restored,
	}, actor)
	return s.alert(ctx, fmt.Sprintf("切流回滚 %s v%s", skillName, row.TargetVer),
		fmt.Sprintf("已切回 v%s（原因：%s）", restored, reason), "critical")
}

// 判 cur 而不是判 err：lookupVersionOptional 在「记录不存在」时返回 (nil, nil)，
// 上面那句 `if err == nil` 恒真，下一行就会对 nil 解引用直接 panic（只登记了 target、
// target_version 还为空就回滚 = 必现）。

// probePrefix 是「最新一批探针」的组名前缀（缺省 = ParityPrefix；测试/巡检可注入覆盖）。
func (s *CutoverService) SetProbePrefix(p string) { s.probePrefix = p }

// Evidence 是某版本在切流窗口内的质量证据。
type Evidence struct {
	Samples     int     // 生产调用样本数
	OKSamples   int     // status=ok 的样本数
	SuccessRate float64 // OKSamples/Samples（无样本 = 0）
	ParityTotal int     // 可比对的探针组数（同组里新旧两版都有记录）
	ParityMatch int     // 结果一致的组数
	ParityRate  float64 // ParityMatch/ParityTotal（无样本 = 0）

	// 最新一批探针（按批次号最大值取）的组数与一致数：首档门槛看这两项。
	LatestBatchTotal int
	LatestBatchMatch int
}

// GateReasons 返回「不允许放量」的原因（空 = 达标）。
func (e Evidence) GateReasons(cfg CutoverConfig) []string {
	cfg = cfg.WithDefaults()
	var out []string
	// 比率比较一律用整数交叉相乘（rate ≤ min ⟺ match*1000 ≤ total*round(min*1000)）：
	// 0.9 / 0.95 这类阈值在二进制浮点里表示不精确，直接比浮点会在边界上抖（54/60 算出的比率
	// 与 0.9 并不严格相等，谁大谁小取决于最后一次舍入）。阈值取千分位参与比较，且门槛是
	// 「严格大于」—— 刚好踩到阈值仍算不达标。
	if e.ParityTotal < cfg.MinSamples {
		out = append(out, fmt.Sprintf("一致率样本不足（%d < %d）", e.ParityTotal, cfg.MinSamples))
	} else if int64(e.ParityMatch)*1000 <= int64(e.ParityTotal)*ratePermille(cfg.ParityMin) {
		out = append(out, fmt.Sprintf("一致率不达标（%d/%d = %.2f ≤ %.2f）",
			e.ParityMatch, e.ParityTotal, e.ParityRate, cfg.ParityMin))
	}
	if e.Samples < cfg.MinSamples {
		out = append(out, fmt.Sprintf("成功率样本不足（%d < %d）", e.Samples, cfg.MinSamples))
	} else if int64(e.OKSamples)*1000 <= int64(e.Samples)*ratePermille(cfg.SuccessMin) {
		out = append(out, fmt.Sprintf("成功率不达标（%d/%d = %.2f ≤ %.2f）",
			e.OKSamples, e.Samples, e.SuccessRate, cfg.SuccessMin))
	}
	return out
}

// ratePermille 把比率阈值换算成千分位整数（0.9 → 900，0.95 → 950）。
func ratePermille(rate float64) int64 {
	return int64(math.Round(rate * 1000))
}

// Satisfied 是 GateReasons 的布尔视图。
func (e Evidence) Satisfied(cfg CutoverConfig) bool { return len(e.GateReasons(cfg)) == 0 }

// FirstStageReasons 是「首次上灰度」的门槛：最新一批对比探针必须跑完且全部一致（离线双跑）。
//
// 与 GateReasons 的区别：
//   - 不看样本量阈值（首档不可能有生产流量样本），只要有一组可比对探针；
//   - 只看「最新一批探针」（run_id 头部批次号最大的那批，见 ParityProbeRunKey）：探针是按批次
//     跑的，修复后重跑的新批次必须能替掉旧批次的结论，否则一次历史的不一致会把首档永久锁死。
//     升档与收尾看的是全窗口总体一致率（GateReasons），历史不一致照样拦着。
//
// 零探针 / 最新批次有任一不一致 = 拒绝。
func (e Evidence) FirstStageReasons() []string {
	var out []string
	if e.LatestBatchTotal == 0 {
		out = append(out, "没有可比对的对比探针（先用同一批入参对新旧实现各跑一次离线双跑）")
	} else if e.LatestBatchMatch < e.LatestBatchTotal {
		out = append(out, fmt.Sprintf("离线双跑存在不一致（%d/%d 组）", e.LatestBatchMatch, e.LatestBatchTotal))
	}
	return out
}

// Evidence 汇总某切流版本的成功率与一致率。
//
// 两个指标的窗口起点不同，这是口径上的必然：
//   - 生产调用（成功率）只看「本档开始之后」的流量（窗口起点 = skill_cutover.stage_at），
//     否则上一档的数据会被重复计入本档，升档依据会被稀释成历史平均值；
//   - 对比探针（一致率）是离线双跑，在放量之前就跑完了，必须不受档位窗口限制，
//     否则首档永远拿不到探针证据（探针在 stage_at 之前写入）。
//
// 尚未放量（stage_at 为空）时生产样本窗口不限时间：首档的门槛是对比探针（见
// FirstStageReasons），成功率窗口只用于升档与收尾。
//
// ⚠️ 两侧都必须显式 Model(&db.SkillRun{})：不指定模型时 GORM 从表名推断实体名，
// 会给子查询生成 `skill_run`.`workflow_id` 这种不存在的限定前缀（sqlite 直接语法报错，
// MySQL 同样不可用），且错误只在 Find 阶段暴露，排查成本极高。
func (s *CutoverService) Evidence(ctx context.Context, skillName, version string) (*Evidence, error) {
	row, err := s.get(ctx, skillName)
	if err != nil {
		return nil, err
	}
	var since *time.Time
	if row != nil {
		since = row.StageAt
	}

	var runs []db.SkillRun
	q := s.db.WithContext(ctx).Model(&db.SkillRun{}).Where("skill_name = ? AND version = ? AND workflow_id <> ?",
		skillName, version, ParityPrefix)
	if since != nil {
		q = q.Where("created_at >= ?", *since)
	}
	if err := q.Order("id").Find(&runs).Error; err != nil {
		return nil, fmt.Errorf("query skill_runs: %w", err)
	}
	ev := &Evidence{}
	for _, r := range runs {
		ev.Samples++
		if r.Status == "ok" {
			ev.OKSamples++
		}
	}
	if ev.Samples > 0 {
		ev.SuccessRate = float64(ev.OKSamples) / float64(ev.Samples)
	}

	var probes []db.SkillRun
	pq := s.db.WithContext(ctx).Model(&db.SkillRun{}).Where("skill_name = ? AND workflow_id = ?", skillName, ParityPrefix)
	if err := pq.Order("id").Find(&probes).Error; err != nil {
		return nil, fmt.Errorf("query parity probes: %w", err)
	}
	byGroup := map[string]map[string]db.SkillRun{}
	for _, r := range probes {
		if versionKeyOfRunKey(r.RunID) == "" {
			continue // 指纹不完整（非本包写入的探针）：不参与一致率，宁可少一个样本
		}
		if byGroup[r.TraceID] == nil {
			byGroup[r.TraceID] = map[string]db.SkillRun{}
		}
		byGroup[r.TraceID][versionKeyOfRunKey(r.RunID)] = r
	}
	// 最新一批探针 = run_id 头部批次号最大的那些组（见 ParityProbeRunKey）。
	latestSeq, hasLatest := -1, false
	for _, r := range probes {
		if n, ok := probeSeqOfRunKey(r.RunID); ok && n > latestSeq {
			latestSeq, hasLatest = n, true
		}
	}
	for _, versions := range byGroup {
		if len(versions) < 2 {
			continue // 单边组：探针没跑完，不算一个可比对样本
		}
		ev.ParityTotal++
		match := ParityMatch(versions)
		if match {
			ev.ParityMatch++
		}
		if hasLatest {
			// 组内任一条记录的批次号命中最新批次，整组就算「最新一批」里的组。
			for _, r := range versions {
				if seq, ok := probeSeqOfRunKey(r.RunID); ok && seq == latestSeq {
					ev.LatestBatchTotal++
					if match {
						ev.LatestBatchMatch++
					}
					break
				}
			}
		} else {
			// 调用方自定组名格式、认不出批次号：首档门槛退化为按总体一致率判。
			ev.LatestBatchTotal++
			if match {
				ev.LatestBatchMatch++
			}
		}
	}
	if ev.ParityTotal > 0 {
		ev.ParityRate = float64(ev.ParityMatch) / float64(ev.ParityTotal)
	}
	return ev, nil
}

// ParityMatch 判定一组对比探针是否「结果一致」（纯函数）。
//
// 入参 key 是版本号指纹；每条记录的结果摘要指纹存放在 Error 列（见 RecordParityRun ——
// skill_runs 没有 output 列，而探针本身不产生错误信息，复用 Error 列存指纹省一次 DDL）。
// 口径：组内每条记录都必须成功（status=ok），且各版本的结果摘要指纹完全相同。
// 任一条失败或摘要缺失 = 整组不一致（缺摘要说明探针没带结果，不能当成一致）。
func ParityMatch(versions map[string]db.SkillRun) bool {
	if len(versions) < 2 {
		return false
	}
	ref := ""
	first := true
	for _, r := range versions {
		if r.Status != "ok" {
			return false
		}
		key := strings.TrimSpace(r.Error)
		if key == "" {
			return false
		}
		if first {
			ref, first = key, false
			continue
		}
		if key != ref {
			return false
		}
	}
	return true
}

// ParitySummaryHash 取「结果摘要」的指纹（sha256 hex，64 字符）；摘要为空 = 无指纹。
//
// 一致率只比这个指纹，**绝不**把延迟/时间戳算进去：同一批入参两次执行的延迟必然不同，
// 把它们算进一致率会让所有组都不一致。
func ParitySummaryHash(summary string) string {
	if summary == "" {
		return "" // 探针没带结果 = 没有可比对的摘要，不能当成一致
	}
	return sha256hex([]byte(summary))
}

// 探针记录的「批次号 + 版本键」编码（skill_runs 没有多余列可用；run_id 是 VARCHAR(64)，
// 所以整串必须定长）：
//
//	run_id = "<4 位零填充批次号>" + sha256(组标识)[:4] + sha256(版本号)[:56]   （定长 64）
//
// 头 4 位就是「这批探针是第几批」；尾部版本键带 4 位组内前缀，保证「同组各版本互不相同、
// 不同组的键互不串台」。定长 + 零填充是关键：变长前缀会让版本键起点漂移，批次号的十进制位
// 还会混进指纹里。批次号从 1 起连续自增 → 「最大批次号」就是最新一批：修复后重跑的批次号
// 更大，新批次的结论替掉旧批次，一次历史不一致不会把首档永久锁死。认不出批次号（外部写入的
// 探针）时退化为「按总体一致率判」，不会误判达标。
const (
	ParityProbeSeqPrefix = "-"
	probeSeqWidth        = 4                                                     // 批次号零填充宽度（定宽才不会让指纹起点漂移）
	probeGroupKeyLen     = 4                                                     // 版本键里的组内前缀长度（让不同组的键互不串台）
	probeVersionKeyLen   = 56                                                    // 版本指纹段长度（4 + 4 + 56 = 64 = run_id 定长）
	probeRunKeyLen       = probeSeqWidth + probeGroupKeyLen + probeVersionKeyLen // run_id 定长 64
	MaxProbeBatchSeq     = 9999                                                  // = 10^probeSeqWidth - 1（批次号溢出必须显式失败，不能回绕）
)

// ParityProbeRunKey 生成探针记录的 run_id（批次号 + 版本号指纹）。
func ParityProbeRunKey(group, version string) string {
	return fmt.Sprintf("%0*d%s", probeSeqWidth, probeSeqOfGroup(group), versionKeyOfGroup(group, version))
}

// versionKeyOfGroup 取「组内版本键」：同一组里各版本必须互不相同，不同组之间互不干扰。
// 变长组名会让「组指纹 + 版本指纹」拼接串的对齐漂移（定宽段不再落在固定偏移），所以先把组
// 标识哈希成定宽前缀再拼版本指纹，并截到定宽 —— 读写两侧都用本函数取键。
func versionKeyOfGroup(group, version string) string {
	return sha256hex([]byte(group))[:probeGroupKeyLen] + sha256hex([]byte(version))[:probeVersionKeyLen]
}

// probeSeqOfGroup 取组标识里的批次号：约定组名 = `<前缀>-<4 位批次号>-<组内序号>`（用
// ParityProbeGroup 生成），批次号段定宽零填充。实现是「从尾部回推」：组内序号段变长，所以
// 先数出末尾连续数字段（组内序号），它前面那个分隔符再往前数定宽 4 位就是批次号段。
//
// 不能简单取首段或尾段：前缀常带日期（`batch-2026-12-05/0001`），只取首段会读到 "2026" 这种
// 伪批次号；只取尾段会把组内序号当批次号，同批各组就散了。认不出（段内有非十进制字符、结构
// 对不上）返回 0，调用方按「认不出」处理 → 首档门槛退化为按总体一致率判，不会误判达标。
func probeSeqOfGroup(group string) int {
	end := len(group)
	for end > 0 && group[end-1] >= '0' && group[end-1] <= '9' {
		end-- // 末尾连续数字段 = 组内序号
	}
	if end == len(group) || end == 0 || group[end-1] != ParityProbeSeqPrefix[0] {
		return 0 // 尾部不是 `-<数字>` 的组内序号段 → 认不出
	}
	start := end - 1 - probeSeqWidth // 跳过组内序号的分隔符，往前取定宽 4 位
	if start < 0 {
		return 0
	}
	digits := group[start : end-1]
	if len(digits) != probeSeqWidth {
		return 0
	}
	seq := 0
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0 // 批次号段不是纯十进制 → 认不出，绝不猜
		}
		seq = seq*10 + int(c-'0')
	}
	if seq <= 0 {
		return 0
	}
	return seq
}

// versionKeyOfRunKey 取探针 run_id 里的版本键部分（头部定宽批次号之后的指纹）。
func versionKeyOfRunKey(runKey string) string {
	if len(runKey) != probeRunKeyLen {
		return ""
	}
	return runKey[probeSeqWidth:]
}

// probeSeqOfRunKey 取探针 run_id 头部的批次号（见 ParityProbeRunKey）。
func probeSeqOfRunKey(runKey string) (int, bool) {
	if len(runKey) != probeRunKeyLen {
		return 0, false
	}
	seq := 0
	for _, c := range runKey[:probeSeqWidth] {
		if c < '0' || c > '9' {
			return 0, false
		}
		seq = seq*10 + int(c-'0')
	}
	if seq <= 0 {
		return 0, false
	}
	return seq, true
}

// ParityProbeGroup 生成一批离线双跑探针的组标识：`<前缀>-<4 位批次号>-<组内序号>`。
//
// 批次号定宽零填充（与 run_id 里的批次号段同宽，见 ParityProbeRunKey），紧跟前缀之后的第一个
// 分隔符：一致率的「最新一批」判定靠它；组内序号从 1 起（让同一批里的各组互不覆盖）。
// 运维脚本 / 巡检生成组名时必须用本函数，不要自己拼字符串 —— 格式不对只会让首档门槛退化成
// 「按总体一致率判」（不会误判达标），但修好之后的新批次就压不住历史不一致，第一档会被卡死。
func ParityProbeGroup(prefix string, batchSeq, index int) string {
	return fmt.Sprintf("%s%s%0*d%s%d", prefix, ParityProbeSeqPrefix, probeSeqWidth, batchSeq, ParityProbeSeqPrefix, index)
}

// ParityProbeKey 生成对比探针的入组键（128 个 hex 字符）= sha256(组标识) + sha256(版本号)。
//
// 为什么不用 skill_runs.workflow_id 存组标识：那一列是 VARCHAR(64)，而组标识是调用方给的
// 任意长度文本（批次号 / 用例名 / UUID 都可能），MySQL 严格模式会直接拒掉；若改成截断，
// 两个长组名会被并成同一个组，一致率就被算成了假数据。所以 workflow_id 只放固定标记
// ParityPrefix（把探针与生产调用分开，口径与 S1 灰度互不污染），精确的组身份放 trace_id
// （CHAR(64) = 组标识指纹），版本号指纹放 run_id（VARCHAR(64)）。读写两侧都用本函数取指纹。
func ParityProbeKey(group, version string) string {
	return sha256hex([]byte(group)) + sha256hex([]byte(version))
}

// RecordParityRun 记录一条对比探针（同一组同一版本各记一条，组内比对时取全部）。
//
// summary 是「可比对的结果摘要」JSON 文本，由调用方按该 skill 的 output_schema 归一
// （x-scan 取 {count, items:[{id,...}]}；like 取 {success_count, results:[{tweet_id,success}]}）。
// ⚠️ 登录态明文绝不允许出现在 summary（调用方负责；本包也绝不打印 summary）。
func (s *CutoverService) RecordParityRun(ctx context.Context, skillName, version, group, inputHash, summary string, ok bool) error {
	if s.db == nil {
		return ErrNoDB
	}
	if group = strings.TrimSpace(group); group == "" {
		return errors.New("parity group 不能为空")
	}
	if len(inputHash) != 64 {
		return fmt.Errorf("input_hash 必须是 sha256 hex（64 字符），得到 %d 字符", len(inputHash))
	}
	if version = strings.TrimSpace(version); version == "" {
		return errors.New("parity 探针必须带版本号（一致率按版本比对）")
	}
	status := "ok"
	if !ok {
		status = "fail"
	}
	run := db.SkillRun{
		SkillName:  skillName,
		Version:    version,
		TraceID:    sha256hex([]byte(group)),          // 组标识指纹（CHAR(64)）：同组各版本共享
		RunID:      ParityProbeRunKey(group, version), // 批次号 + 版本键（定长 64，见 ParityProbeRunKey）
		WorkflowID: ParityPrefix,                      // 固定标记：把探针与生产调用分开，且不撑爆 VARCHAR(64)
		InputHash:  inputHash,                         // 64 位入参指纹：同组各版本必须同一入参
		Error:      ParitySummaryHash(summary),
		Status:     status,
		CreatedAt:  s.now(),
	}
	if err := s.db.WithContext(ctx).Create(&run).Error; err != nil {
		return fmt.Errorf("insert parity probe: %w", err)
	}
	return nil
}

// ensureTargetVersion 取/建切流版本行（config.addr = 目标 addr）。返回 (版本, 是否新建)。
//
// 只允许改写「本切流登记过的版本」：
//   - 版本号 == row.TargetVer（本切流已在用的目标版本）→ 允许刷新 config.addr；
//   - 版本号从未登记过切流（row.TargetVer 为空 = 本次是首档）→ 允许，这就是首档要建的新行；
//   - 其余情况（版本号是别的现存版本，尤其是 live/带流量的基线）→ 拒绝。
//
// 为什么必须挡（高危）：网关按**版本行 config 里的 addr** 路由（skillgateway/runner.go 的
// VersionConfigAddr），而本函数会把 config.addr 改写成切流目标。若允许把 stage 的 version
// 填成现存基线版本号，流量就在任何质量护栏执行之前被打到新实现上：后续 GateReasons 拒绝
// 也拦不住（config 已落库、审计里只有一条被拒绝的调用），线上表现为「护栏说没放量、
// 真实流量已经切过去」。要切基线版本请走正常的注册/灰度链（S1），不是切流。
func (s *CutoverService) ensureTargetVersion(ctx context.Context, skill *db.Skill, row *db.SkillCutover, version string) (*db.SkillVersion, bool, error) {
	if version = strings.TrimSpace(version); version == "" {
		version = row.TargetVer
	}
	// 空版本号必须在这里就拒掉，绝不能往下走「新建版本行」分支：那会建出 version='' 的
	// skill_versions 行，并把 skill_cutover.target_version 落成 ''。后果不是脏数据而是**护栏失效**：
	// approval.cutoverTargetVersions 用 `target_version <> '' AND status IN (pending,running)`
	// 判「这一行是不是切流版本」，空 target_version 让该 gray 行被当成普通 S1 灰度接管，
	// advanceGray 的 switch 对 gray_stage=10 落到 default（语义 = 100% 窗口健康 → 完成）→
	// 直接置 live/100 并 retire 基线，正是本文件反复警告的「绕过人工护栏全量」。
	// routes.setStage 的 version 不是必填，所以这条路径在 HTTP 层可达。
	if version == "" {
		return nil, false, fmt.Errorf(
			"%w: 切流首档必须显式指定 version（空版本号会建出 version 为空的版本行，"+
				"并把 skill_cutover.target_version 落成空 → approval.TickGray 不再让路，灰度状态机会把该版本直接推成 live/100%%）",
			ErrCutoverStage)
	}
	var v db.SkillVersion
	err := s.db.WithContext(ctx).Where("skill_id = ? AND version = ?", skill.ID, version).First(&v).Error
	if err == nil {
		// 判「是否带流量」，不判 TargetVer 是否为空：首档时 TargetVer 还空着，只看 TargetVer
		// 会让「把 stage 的 version 填成 live 基线」直接放行（正是最危险的那条路）。
		// 允许改写 config.addr 的只有两类：本切流已登记的版本（TargetVer == version，含回滚后
		// 重放），以及从未进过路由的零流量版本（draft/testing/retired，traffic_split=0 ——
		// 网关 ResolveLive 只认 status IN (live,gray) AND traffic_split > 0，改它们不产生线上影响；
		// 首档「护栏先跑、版本行后建」的重放路径也依赖这一条）。
		// 只认「已登记的零流量切流行」这一种重放：status=gray 且 traffic_split=0 = 本切流首档
		// 被护栏拒绝时建出来的那一行（见 ensureTargetVersion 的归零），重放时允许复用；
		// live / 带流量的版本一律禁止（那才是「绕过护栏切流量」）。
		if row.TargetVer != version && !(v.Status == "gray" && v.TrafficSplit == 0) {
			return nil, false, fmt.Errorf(
				"%w: %s 是现存 %s 版本（流量 %d%%），切流只能改写「本切流登记过的目标版本」或「从未进过路由的零流量版本」；改写带流量的版本会让流量绕过质量护栏直接打到新实现",
				ErrCutoverVersionLocked, version, v.Status, v.TrafficSplit)
		}
		cfg, cErr := mergeConfigAddr(v.Config, row.TargetAddr)
		if cErr != nil {
			return nil, false, cErr
		}
		v.Config = cfg
		if v.Status == "" || v.Status == "draft" || v.Status == "testing" {
			v.Status = "gray"
		}
		if err := s.db.WithContext(ctx).Save(&v).Error; err != nil {
			return nil, false, fmt.Errorf("update cutover version: %w", err)
		}
		// Save 之后必须显式归零（与新建分支同口径）：TrafficSplit 带 gorm `default:100`，
		// GORM 的 UPDATE 同样会跳过零值字段 —— 首档被护栏拒绝时版本行已经建出来（零流量），
		// 重放到这一分支若不归零，这行 gray 版本就永远停在 DB 默认的 100%。
		if err := s.db.WithContext(ctx).Model(&v).Update("traffic_split", v.TrafficSplit).Error; err != nil {
			return nil, false, fmt.Errorf("sync cutover traffic: %w", err)
		}
		s.log(ctx, "cutover_version_config", fmt.Sprintf("%s v%s", skill.Name, v.Version), map[string]any{
			"addr": row.TargetAddr, "status": v.Status, "traffic_split": v.TrafficSplit,
		}, s.cfg.Actor)
		return &v, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("query skill version: %w", err)
	}

	var live db.SkillVersion
	err = s.db.WithContext(ctx).
		Where("skill_id = ? AND status = ? AND traffic_split > 0", skill.ID, "live").
		Order("id DESC").First(&live).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("query live version: %w", err)
	}
	if live.Image == "" {
		return nil, false, fmt.Errorf("skill %s 无 live 版本可继承镜像/config（先注册基线版本）", skill.Name)
	}
	cfg, err := mergeConfigAddr(live.Config, row.TargetAddr)
	if err != nil {
		return nil, false, err
	}
	v = db.SkillVersion{
		SkillID:      skill.ID,
		Version:      version,
		Image:        live.Image, // 同源能力：镜像继承基线版本，行为差异只由 addr 指向的新实现带来
		Config:       cfg,        // 契约快照继承基线（input/output schema 相同），只换 addr
		TrafficSplit: 0,          // 零流量建行：放量只由 applyTraffic 在护栏之后写
		Status:       "gray",
		CreatedAt:    s.now(),
	}
	// Select 显式点名 traffic_split：该列带 gorm `default:100`，GORM 的默认字段写入策略
	// (InsertOnZero) 会跳过带 default 标签的零值 → 落库变 100。点名之后建行走「显式列」路径，
	// 值取上面结构体里的真实零值（不依赖字段类型），下面那条 UPDATE 只是同值的二次确认。
	if err := s.db.WithContext(ctx).Select("skill_id", "version", "image", "config", "traffic_split", "status", "created_at").
		Create(&v).Error; err != nil {
		return nil, false, fmt.Errorf("create cutover version: %w", err)
	}
	// 显式把 traffic_split 归零（高危）：SkillVersion.TrafficSplit 带 gorm `default:100`，
	// GORM 对带 default 标签的零值字段**不下发该列**，落库取 DB 默认 100（内存里仍是 0）。
	// 这一行 Status=gray 且 config.addr 已指向切流目标，而网关 ResolveLive 按
	// 「status IN (live,gray) AND traffic_split > 0」加权路由 —— 不显式归零的话：
	//   - 首档护栏（FirstStageReasons）还没跑，新实现就已经参与路由；
	//   - 首档被护栏拒绝（探针缺失是最常见情况）后，这行会以 gray + 100% 永久留在库里，
	//     与基线 live 版本各占一半 → 50% 生产流量被静默切到未验证的实现，且后续档位无法自愈。
	if err := s.db.WithContext(ctx).Model(&v).Update("traffic_split", 0).Error; err != nil {
		return nil, false, fmt.Errorf("reset cutover traffic: %w", err)
	}
	v.TrafficSplit = 0
	return &v, true, nil
}

// applyTraffic 写档位流量：切流版本 = traffic，其余有流量的版本按比例承接剩余（合计 100）。
// 返回被切流的旧版本号（无旧版本时为空）。
func (s *CutoverService) applyTraffic(ctx context.Context, skillID uint, v *db.SkillVersion, traffic int) (string, error) {
	var others []db.SkillVersion
	if err := s.db.WithContext(ctx).
		Where("skill_id = ? AND id <> ? AND status IN ? AND traffic_split > 0",
			skillID, v.ID, []string{"live", "gray"}).
		Order("id").Find(&others).Error; err != nil {
		return "", fmt.Errorf("query other versions: %w", err)
	}

	v.TrafficSplit = traffic
	v.Status = "gray"
	stage := traffic
	v.GrayStage = &stage
	if err := s.db.WithContext(ctx).Save(v).Error; err != nil {
		return "", fmt.Errorf("update cutover traffic: %w", err)
	}
	// Save 之后逐列显式下发 traffic_split：TrafficSplit 带 gorm `default:100`，GORM 的
	// INSERT/UPDATE 会跳过带 default 标签的**零值**字段。10/50/100 三档都非零时看不出来，
	// 但「回滚后重放」「档位归零」这类路径一旦走到这里，DB 里的值就会与内存对象不一致 ——
	// 而网关只认库里的数（ResolveLive 按 traffic_split > 0 加权路由）。多一条 UPDATE 的成本，
	// 换「内存里看到什么，库里就是什么」这条不变量。
	if err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
		Where("id = ?", v.ID).Update("traffic_split", v.TrafficSplit).Error; err != nil {
		return "", fmt.Errorf("set cutover traffic: %w", err)
	}
	if len(others) == 0 {
		return "", nil
	}

	remaining := 100 - traffic
	total := 0
	for _, o := range others {
		total += o.TrafficSplit
	}
	if total <= 0 {
		total = len(others)
	}
	// 承接量按各旧版本的现有流量等比分配，末位只补「还剩多少没分」——不是补 remaining。
	// 末位赋 remaining 只在「只有一个旧版本」时成立（此时它自己就是全部权重）；同一 skill
	// 有 ≥2 个带流量的版本时（S1 灰度期间 live(95%)+gray(5%) 是常态），各项之和会变成
	// remaining*(1+Σ非末位权重/total) > remaining：例如 others=[95,5]、traffic=10 →
	// 85 + 90 = 175，加上新实现的 10 共 185%，网关按比例加权路由 = 超发抢量。
	assigned := 0
	last := len(others) - 1
	for i := range others {
		share := remaining * others[i].TrafficSplit / total
		if i == last {
			share = remaining - assigned // 末位兜住整除余数，保证 Σshare == remaining
		}
		assigned += share
		others[i].TrafficSplit = share
		if share == 0 {
			others[i].Status = "retired"
		}
		if err := s.db.WithContext(ctx).Save(&others[i]).Error; err != nil {
			return "", fmt.Errorf("update legacy traffic: %w", err)
		}
		// 同上：share=0（承接后彻底退出路由）时 Save 不会下发 traffic_split 这一列，
		// 旧实现就会带着原流量继续与新实现分流量 —— 必须显式写 0。
		if err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
			Where("id = ?", others[i].ID).Update("traffic_split", share).Error; err != nil {
			return "", fmt.Errorf("update legacy traffic: %w", err)
		}
	}
	return others[0].Version, nil
}

func (s *CutoverService) get(ctx context.Context, skillName string) (*db.SkillCutover, error) {
	if s.db == nil {
		return nil, ErrNoDB
	}
	var row db.SkillCutover
	err := s.db.WithContext(ctx).Where("skill_name = ?", skillName).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query skill_cutover: %w", err)
	}
	return &row, nil
}

// lookupVersion 按 skill 名 + 版本号取版本行（找不到 = ErrCutoverNoTarget）。
func (s *CutoverService) lookupVersion(ctx context.Context, skillName, version string) (*db.SkillVersion, error) {
	v, err := s.lookupVersionOptional(ctx, skillName, version)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("%w: %s v%s", ErrCutoverNoTarget, skillName, version)
	}
	return v, nil
}

// lookupVersionOptional 同 lookupVersion，但找不到返回 (nil, nil)。
func (s *CutoverService) lookupVersionOptional(ctx context.Context, skillName, version string) (*db.SkillVersion, error) {
	if s.versionLookup != nil {
		return s.versionLookup(ctx, skillName, version)
	}
	var v db.SkillVersion
	err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
		Where("skill_id = (SELECT skills.id FROM skills WHERE skills.name = ?) AND skill_versions.version = ?", skillName, version).
		First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query target version: %w", err)
	}
	return &v, nil
}

// lookupAnyVersion 按 skill 名 + 版本号取版本行，**不看 status**，找不到返回 (nil, nil)。
//
// 与 lookupVersionOptional 的区别：后者的注入版（测试）与生产版语义都是「按名字取那一行」，
// 回滚里恢复旧版本必须用不带 status 过滤的查询 —— 收尾（Complete）会把其余版本行写成
// retired + traffic_split=0，而 legacy_version 记的正是那一行。
func (s *CutoverService) lookupAnyVersion(ctx context.Context, skillName, version string) (*db.SkillVersion, error) {
	if s.anyVersionLookup != nil {
		return s.anyVersionLookup(ctx, skillName, version)
	}
	var v db.SkillVersion
	// 关联子查询：sqlite 不支持 `skill_id = (SELECT ...)` 这种写法（生产是 MySQL，走这条），
	// 与 lookupLegacyVersion 同款限制；测试用 legacyLookup 注入等价语义。
	err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
		Where("skill_id = (SELECT skills.id FROM skills WHERE skills.name = ?) AND skill_versions.version = ?", skillName, version).
		First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query version %s: %w", version, err)
	}
	return &v, nil
}

// liveTrafficOf 返回该 skill 在路由池里的总流量：只统计网关认的那一套条件
// （status IN (live,gray) AND traffic_split > 0），并排除 excludeID（本次正在被摘流量的那一行）。
//
// 用它而不是「行数」判空：路由池是否有东西可路由，取决于流量之和（网关 ResolveLive 按各行
// traffic_split 归一加权），行数会受「多版本同时带流量」影响。
func (s *CutoverService) liveTrafficOf(ctx context.Context, skillID uint, excludeID uint) (int, error) {
	var sum *int
	err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
		Where("skill_id = ? AND id <> ? AND status IN ? AND traffic_split > 0",
			skillID, excludeID, []string{"live", "gray"}).
		Select("COALESCE(SUM(traffic_split), 0)").Scan(&sum).Error
	// 注：Scan 到 *int 时 GORM 在零行聚合下返回 nil（COALESCE 只在有行时生效），下面兜 0。
	if err != nil {
		return 0, fmt.Errorf("sum live traffic: %w", err)
	}
	if sum == nil {
		return 0, nil
	}
	return *sum, nil
}

// lookupLegacyVersion 取「除切流版本之外」最早的版本行（回滚时恢复它）。
func (s *CutoverService) lookupLegacyVersion(ctx context.Context, skillName, excludeVersion string) (*db.SkillVersion, error) {
	if s.legacyLookup != nil {
		return s.legacyLookup(ctx, skillName, excludeVersion)
	}
	var old db.SkillVersion
	err := s.db.WithContext(ctx).Model(&db.SkillVersion{}).
		Where("skill_id = (SELECT skills.id FROM skills WHERE skills.name = ?) AND skill_versions.version <> ?", skillName, excludeVersion).
		Order("skill_versions.id ASC").First(&old).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query legacy version: %w", err)
	}
	return &old, nil
}

// log 写审计留痕（audit_logs 只增不删；写失败不改主流程返回值，由日志暴露）。
func (s *CutoverService) log(ctx context.Context, action, target string, detail any, actor string) {
	if s.db == nil {
		return // 无库 = 无审计面（调用方已在入口用 ErrNoDB 挡掉写操作，这里只兜底）
	}
	if actor == "" {
		actor = s.cfg.Actor
	}
	if d, ok := detail.(map[string]any); ok && s.auditFn != nil {
		// 单测注入的审计面：让「每一步都留痕」可断言（生产走 DB，行为一致）。
		if err := s.auditFn(ctx, actor, action, target, d); err == nil {
			return
		}
	}
	entry := db.AuditLog{Actor: actor, Action: action, Target: target}
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			entry.Detail = string(b)
		}
	}
	if err := s.db.WithContext(ctx).Create(&entry).Error; err != nil {
		fmt.Printf("WARN: migration audit (%s %s): %v\n", action, target, err)
	}
}

// ValidStage 判断档位是否合法。
func ValidStage(traffic int) bool {
	for _, s := range CutoverStages {
		if s == traffic {
			return true
		}
	}
	return false
}

// StagesLabel 返回档位清单文本（错误信息用）。
func StagesLabel() string {
	out := make([]string, 0, len(CutoverStages))
	for _, s := range CutoverStages {
		out = append(out, fmt.Sprintf("%d", s))
	}
	return strings.Join(out, "/")
}

// mergeConfigAddr 在版本 config 快照里改写 addr（其余字段原样保留）。
// configAddrOf 取版本 config 里的 addr（解析失败/缺字段返回空串，由调用方决定怎么兜底）。
func configAddrOf(configJSON string) string {
	if strings.TrimSpace(configJSON) == "" {
		return ""
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return ""
	}
	if v, ok := cfg["addr"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func mergeConfigAddr(configJSON, addr string) (string, error) {
	cfg := map[string]any{}
	if strings.TrimSpace(configJSON) != "" && configJSON != "null" {
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return "", fmt.Errorf("version config 不是 JSON object: %w", err)
		}
	}
	cfg["addr"] = addr
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal version config: %w", err)
	}
	return string(b), nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
