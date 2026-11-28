// decommission.go：旧服务缩容下线清单（M4-1）。
//
// 切流到 100% 并观察一个窗口后，pycraper（x-scan 的旧实现）与 pyreplay（like 的旧实现）
// 就可以退出 skill 调用链。本文件只做一件事：**产出一份可核对的缩容/下线计划**
// （每个服务一条动作 + 阻断项），执行由运维按清单做（deployctl / docker compose scale）。
//
// 为什么不自动停：pycraper / pyreplay 同时还是 Temporal worker（抓取与点赞的 activity
// 队列、like_jobs 后台执行器、M1-4 的 login_worker 消费端），它们承载的不只是这两个
// skill。下线 = 一次性摘掉整条 worker 链，风险由人工承担更合适；本包的职责是把
// 「现在能不能缩、缩到什么程度、还差什么」算清楚并留痕。
package migration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// 缩容动作。
const (
	ActionScaleDown = "scale_down" // 副本数下调（灰度期降载）
	ActionStop      = "stop"       // 停止服务（下线）
	ActionKeep      = "keep"       // 保持不动（条件不满足）
)

// DecommissionService 是旧服务的下线评估器。
type DecommissionService struct {
	db           *gorm.DB
	cfg          CutoverConfig
	stableWindow time.Duration
	now          func() time.Time
}

// NewDecommission 构造下线评估器。stableWindow = 100% 档需要稳定观察的时长
// （缺省 24h：一个完整的业务日，覆盖定时任务与限流窗口）。
func NewDecommission(gdb *gorm.DB, cfg CutoverConfig, stableWindow time.Duration) *DecommissionService {
	if stableWindow <= 0 {
		stableWindow = 24 * time.Hour
	}
	return &DecommissionService{db: gdb, cfg: cfg.WithDefaults(), stableWindow: stableWindow, now: time.Now}
}

// LegacyService 是一个旧服务在一项 skill 里的角色。
type LegacyService struct {
	Service string // compose 服务名（pycraper / pyreplay）
	Skill   string // 对应 skill（x-scan / like）
	Addr    string // 该 skill 的旧实现 addr
}

// DefaultLegacyServices 是 M4-1 收口的两条能力对应的旧服务。
func DefaultLegacyServices() []LegacyService {
	return []LegacyService{
		{Service: "pycraper", Skill: "x-scan", Addr: "http://pycraper:9504"},
		{Service: "pyreplay", Skill: "like", Addr: "http://pyreplay:9500"},
	}
}

// ServicePlan 是一个旧服务的缩容/下线判定。
type ServicePlan struct {
	Service     string   `json:"service"`
	Skill       string   `json:"skill"`
	Action      string   `json:"action"`       // scale_down | stop | keep
	Replicas    int      `json:"replicas"`     // 建议副本数（0 = 下线）
	Blocked     []string `json:"blocked"`      // 阻断项（非空 = ActionKeep）
	Evidence    Evidence `json:"evidence"`     // 该 skill 新实现的质量证据
	LegacyCalls int64    `json:"legacy_calls"` // 观察窗内仍打到旧 addr 的调用数
	Note        string   `json:"note"`
}

// Plan 是全部旧服务的下线计划。
type Plan struct {
	Services []ServicePlan `json:"services"`
	Ready    bool          `json:"ready"` // 全部服务都无阻断项
	Window   string        `json:"window"`
}

// Ready 判断某服务是否可下线（阻断项为空）。
func (p ServicePlan) Ready() bool { return len(p.Blocked) == 0 }

// Text 渲染人读清单（CLI / 告警用）。
func (pl Plan) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "旧服务下线计划（观察窗 %s）\n", pl.Window)
	for _, s := range pl.Services {
		fmt.Fprintf(&b, "- %s（skill=%s）→ %s，副本 %d", s.Service, s.Skill, s.Action, s.Replicas)
		if len(s.Blocked) > 0 {
			fmt.Fprintf(&b, "【阻断】%s", strings.Join(s.Blocked, "；"))
		}
		fmt.Fprintf(&b, "｜成功率 %.2f（%d 样本）一致率 %.2f（%d 组）旧链路调用 %d\n",
			s.Evidence.SuccessRate, s.Evidence.Samples, s.Evidence.ParityRate, s.Evidence.ParityTotal, s.LegacyCalls)
		if s.Note != "" {
			fmt.Fprintf(&b, "  %s\n", s.Note)
		}
	}
	if pl.Ready {
		b.WriteString("结论：全部满足，可按清单执行（人工）。")
	} else {
		b.WriteString("结论：存在阻断项，保持现状。")
	}
	return b.String()
}

// Evaluate 逐服务判定：能缩到几台 / 能不能停 / 还差什么。
//
// 判定顺序（任一不满足 = keep + 阻断项）：
//  1. skill 已登记切流且状态是 completed（100% 且已收尾）；
//  2. 收尾后稳定观察满 stableWindow；
//  3. 新实现的质量证据达标（一致率 + 成功率，零证据不放行）；
//  4. 观察窗内没有任何生产调用仍打到旧 addr（skill_versions 里旧版本 traffic=0 且 retired）。
func (d *DecommissionService) Evaluate(ctx context.Context, services []LegacyService) (*Plan, error) {
	if len(services) == 0 {
		services = DefaultLegacyServices()
	}
	plan := &Plan{Window: d.stableWindow.String()}
	plan.Ready = true
	for _, svc := range services {
		sp, err := d.evaluateOne(ctx, svc)
		if err != nil {
			return nil, err
		}
		plan.Services = append(plan.Services, *sp)
		if !sp.Ready() {
			plan.Ready = false
		}
	}
	return plan, nil
}

func (d *DecommissionService) evaluateOne(ctx context.Context, svc LegacyService) (*ServicePlan, error) {
	sp := &ServicePlan{Service: svc.Service, Skill: svc.Skill, Action: ActionKeep, Replicas: 1}

	// 每个返回路径都带上「观察窗内旧链路还在接多少调用」，包括被阻断的路径：
	// 旧链路明明还有流量，恰恰是运维最需要立刻看到的数字，不能被阻断项挡掉。
	// 取数失败一律**显式判阻断**（见 setCalls 内注释）：缺数 = 未知，而停服准入条件是
	// LegacyCalls == 0 —— 只写 Note、让字段保持零值，就等于把「未知」读成「零调用」。
	setCalls := func(sp *ServicePlan, skill string, since time.Time) *ServicePlan {
		n, err := d.LegacyCallCount(ctx, skill, since)
		if err != nil {
			// 计数读不出来 = 未知。一律显式阻断（不只是写进 Note）：停服准入条件是
			// LegacyCalls == 0，任何「保持零值继续往下判」的写法都会把未知读成零调用。
			sp.Blocked = append(sp.Blocked, fmt.Sprintf("旧链路调用数取数失败（无法确认已无残余流量）：%v", err))
			return sp
		}
		sp.LegacyCalls = n
		return sp
	}

	cut := &CutoverService{db: d.db, cfg: d.cfg, now: d.now}
	row, err := cut.get(ctx, svc.Skill)
	if err != nil {
		return nil, err
	}
	if row == nil {
		sp.Blocked = append(sp.Blocked, fmt.Sprintf("skill %s 未登记切流目标", svc.Skill))
		return setCalls(sp, svc.Skill, d.now().Add(-d.stableWindow)), nil
	}
	// 证据取数失败 = 「达标与否未知」，同「计数取数失败」一样必须判阻断并返回：
	// 下面的停服判定要求 GateReasons 为空（零证据会被判不达标），若这里放行空 Evidence，
	// 一次查询故障就能让「质量证据」这一关形同虚设。
	ev, err := cut.Evidence(ctx, svc.Skill, row.TargetVer)
	if err != nil {
		sp.Blocked = append(sp.Blocked, fmt.Sprintf("质量证据取数失败（达标与否未知）：%v", err))
		return setCalls(sp, svc.Skill, d.now().Add(-d.stableWindow)), nil
	}
	if ev != nil {
		sp.Evidence = *ev
	}

	cfg := d.cfg.effective(row)
	switch row.Status {
	case CutoverCompleted:
		// ok，继续看观察窗
	case CutoverRolledBack:
		sp.Blocked = append(sp.Blocked, "切流已回滚（rolled_back），旧服务必须保持在线")
		return setCalls(sp, svc.Skill, d.now().Add(-d.stableWindow)), nil
	default:
		sp.Blocked = append(sp.Blocked, fmt.Sprintf("切流未收尾（status=%s traffic=%d%%），完成 Complete 后才能缩容",
			row.Status, row.Traffic))
		return setCalls(sp, svc.Skill, d.now().Add(-d.stableWindow)), nil
	}

	if reasons := sp.Evidence.GateReasons(cfg); len(reasons) > 0 {
		sp.Blocked = append(sp.Blocked, "质量证据不达标："+strings.Join(reasons, "；"))
		return setCalls(sp, svc.Skill, d.now().Add(-d.stableWindow)), nil
	}

	// 稳定观察窗：起点 = 收尾时刻（Complete 写 completed_at）。缺列（老库未迁移）时
	// 退回 updated_at，宁可多等也不放过窗口。
	since := row.UpdatedAt
	if row.CompletedAt != nil {
		since = *row.CompletedAt
	}
	if since.IsZero() {
		since = row.CreatedAt
	}
	// ⚠️ 停服准入路径上的取数失败必须**判阻断并返回**，不能只写 Note：
	// 下面唯一的停服条件是 `sp.LegacyCalls > 0`，取数失败时它保持零值 —— 于是「未知」被读成
	// 「旧链路零调用」，清单会直接给出 ActionStop / Replicas 0 +「可停 compose 服务」的结论。
	// 停服是不可逆动作（旧容器一停，未被网关管住的直连调用方立刻全挂），计数只能往保守方向偏：
	// DB 抖动 / 表缺失 / 权限问题一律降级为「无法确认已无残余流量」→ 缩容到 1 副本并要人工定位。
	calls, err := d.LegacyCallCount(ctx, svc.Skill, since)
	if err != nil {
		sp.Action = ActionScaleDown
		sp.Replicas = 1
		sp.Blocked = append(sp.Blocked, fmt.Sprintf("旧链路调用数取数失败（无法确认已无残余流量）：%v", err))
		sp.Note = strings.TrimSpace(sp.Note + " 计数取不到时一律按「有残余流量」处理：先修数据源（skill_runs / 数据库），确认归零后再停服。")
		return sp, nil
	}
	sp.LegacyCalls = calls
	if d.now().Sub(since) < d.stableWindow {
		sp.Action = ActionScaleDown
		sp.Replicas = 1
		sp.Blocked = append(sp.Blocked, fmt.Sprintf("100%% 稳定观察未满（已 %s / 需 %s）",
			d.now().Sub(since).Truncate(time.Minute), d.stableWindow))
		sp.Note = "可先把副本降到 1（灰度降载），观察满窗口再停。"
		return sp, nil
	}

	// 旧实现是否真的退出路由：旧版本行必须 traffic_split=0（Complete 已 retired）。
	var legacyRunning int64
	if err := d.db.WithContext(ctx).Model(&db.SkillVersion{}).
		Where("skill_id = (SELECT id FROM skills WHERE name = ?) AND traffic_split > 0 AND version <> ?",
			svc.Skill, row.TargetVer).
		Count(&legacyRunning).Error; err != nil {
		return nil, fmt.Errorf("count legacy versions: %w", err)
	}
	if legacyRunning > 0 {
		sp.Blocked = append(sp.Blocked, fmt.Sprintf("仍有 %d 个旧版本带流量（traffic_split>0）", legacyRunning))
		return setCalls(sp, svc.Skill, since), nil
	}
	// 收尾之后旧实现不再被路由（skills.addr 已收敛 + 旧版本行 traffic=0），所以观察窗内
	// skill_runs 里**不该再有**该 skill 的生产调用。还有 = 存在没被网关管住的调用方（直连旧
	// addr / workflow 里写死的地址 / 老容器仍在跑），此时停服会让这些调用直接失败。
	if sp.LegacyCalls > 0 {
		sp.Action = ActionScaleDown
		sp.Replicas = 1
		sp.Blocked = append(sp.Blocked, fmt.Sprintf("观察窗内旧链路仍有 %d 次调用", sp.LegacyCalls))
		sp.Note = strings.TrimSpace(sp.Note + " 先定位并改掉残余调用方（切到 skill 网关），计数归零后才能停服。")
		return sp, nil
	}

	sp.Action = ActionStop
	sp.Replicas = 0
	sp.Note = fmt.Sprintf("skill %s 已全量切到 %s 并稳定观察满 %s，可停 compose 服务 %s（人工执行：cd deploy && docker compose stop %s）。"+
		"注意 %s 还承载 Temporal worker 与登录消费链，停服前确认无在跑任务。",
		svc.Skill, row.TargetAddr, d.stableWindow, svc.Service, svc.Service, svc.Service)
	return sp, nil
}

// LegacyCallCount 统计某 skill 在观察窗内**打到旧实现**的生产调用数（不含对比探针），用于
// 核对「旧链路是否真的没有流量了」。计数口径与 evaluateOne 的旧链路判定完全一致（两处必须
// 一样，否则运维在 CLI 上数出来的数与计划里的阻断项对不上）：
//
//   - version <> 切流目标版本：切流目标 = 新实现（social），它之外的每一个版本都是旧实现
//     （pycraper / pyreplay）。旧实现每接一次流量就多一行，这些行就是停服前必须清零的读数。
//     只数「非目标版本」而不是「目标版本 + completed_at 之后」：收尾之前目标版本的调用本来就
//     存在（那正是新实现承接流量的证据），拿时间窗当主判据会把证据与残余流量混成一谈；
//   - 排除对比探针（workflow_id 以 ParityPrefix 开头）：探针是离线双跑，收尾之后运维照常会
//     补跑几组验证一致性，把它们算成「旧链路还在接流量」会让停服永远被阻断。
//
// 未登记切流目标（或没有目标版本）时退回「全部生产调用」：连基线版本都认不出来时，宁可多报
// 也不漏报 —— 停服是不可逆动作，计数只会往保守方向偏。
func (d *DecommissionService) LegacyCallCount(ctx context.Context, skillName string, since time.Time) (int64, error) {
	if d.db == nil {
		return 0, ErrNoDB
	}
	target, err := d.cutoverTargetVersion(ctx, skillName)
	if err != nil {
		return 0, err
	}
	q := d.db.WithContext(ctx).Model(&db.SkillRun{}).
		Where("skill_name = ? AND created_at >= ? AND workflow_id NOT LIKE ?", skillName, since, ParityPrefix+"%")
	if target != "" {
		q = q.Where("version <> ?", target)
	}
	var n int64
	err = q.Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("count skill_runs: %w", err)
	}
	return n, nil
}

// cutoverTargetVersion 取某 skill 的切流目标版本号（未登记切流 = ""）。
func (d *DecommissionService) cutoverTargetVersion(ctx context.Context, skillName string) (string, error) {
	var row db.SkillCutover
	err := d.db.WithContext(ctx).Model(&db.SkillCutover{}).Where("skill_name = ?", skillName).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query skill_cutover: %w", err)
	}
	return row.TargetVer, nil
}
