// Package migration 是 M4「pycraper / pyreplay → social 收口」的执行面（工作项
// wi-1795489658-2d59 / M4-1）：
//
//   - 会话保活（keepalive.go）：定时心跳检测渠道登录态掉线 → 账号置 need_login + 告警；
//   - 跨服务切流（cutover.go）：skill 网关 x-scan / like 的 addr 指向 social，
//     traffic_split 10% → 50% → 100% 逐档放量，每档看「一致率 + 成功率」达标才升档；
//   - 旧服务下线（decommission.go）：100% 稳定一个观察窗后产出 pycraper / pyreplay
//     的缩容下线清单（含逐服务阻断项），绝不自动停核心服务。
//
// 三条红线：
//  1. 保活只降级不升级：掉线判定只把账号推向 need_login，**绝不**把 need_login/banned
//     改回 active（登录态恢复只能由登录回报链写入）；banned 是人工终态。
//  2. 切流是人工决策 + 自动护栏：写路由必须挂鉴权（见 routes.go），每次切流/回滚/下线
//     都写 audit_logs（只增不删）。
//  3. 旧服务下线是「建议 + 人工执行」：本包只产出计划与阻断项（未达 100% / 证据不达标
//     = 禁止缩容），执行由运维按清单做。
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

// 账号状态机取值（与 apps/social/internal/store 的 Account* 常量逐字一致）。
const (
	statusActive    = db.SocialAccountActive
	statusNeedLogin = db.SocialAccountNeedLogin
	statusBanned    = db.SocialAccountBanned
	statusCooldown  = db.SocialAccountCooldown
)

// ErrNoDB 未接数据库：所有读写基础设施的入口必须先挡这一刀。
// 为什么必须显式挡：*gorm.DB 是普通指针，nil 时 gorm 的方法会直接空指针 panic，
// 把「配置漏了」变成一次进程崩溃（在 audit ticker 里 = 整个巡检进程挂掉）。
var ErrNoDB = errors.New("migration: 未接数据库（*gorm.DB 为 nil）")

// AccountSnapshot 是保活判定需要的账号视图（纯数据，不含任何登录态明文）。
//
// SessionRef 只用来判断「这个账号有没有登录态」，绝不进日志/告警/审计。
type AccountSnapshot struct {
	Channel       string
	AccountID     string
	Status        string
	SessionRef    string
	LastLoginAt   *time.Time
	CooldownUntil *time.Time
}

// KeepAliveStore 是保活巡检需要的审计留痕面（*approval.Store 的方法集是其超集）。
type KeepAliveStore interface {
	Log(ctx context.Context, actor, action, target string, detail any) error
}

// AccountProbe 返回某渠道在账号池里的账号快照（只读）。
type AccountProbe func(ctx context.Context, channel string) ([]AccountSnapshot, error)

// AccountDropper 把掉线账号置 need_login（附原因）。返回 true = 本次真的发生了状态变更。
type AccountDropper func(ctx context.Context, channel, accountID, reason string) (bool, error)

// AlertFunc 发告警（调用方用 notify.Notifier.SendAlert 适配）。
type AlertFunc func(ctx context.Context, title, body, level string) error

// KeepAliveConfig 是保活巡检的运行配置（见 config.go 的 EnvConfig.KeepAliveConfig）。
type KeepAliveConfig struct {
	// Channels 是保活覆盖的渠道清单（缺省 x）。
	Channels []string
	// StaleAfter 是「最近一次成功登录距今超过该时长」的阈值：超过 = 会话已失效。
	StaleAfter time.Duration
	// MaxDropsPerTick 是单轮最多降级的账号数。
	// 取小值（缺省 50）：探测链路自身故障时，一轮把整池账号打成 need_login 会让点赞/
	// 发布链全停（比掉线本身更糟），必须留人工确认的余地。
	MaxDropsPerTick int
	// Actor 是审计留痕的操作者身份。
	Actor string
}

// KeepAliveResult 是一轮保活巡检的产出。
type KeepAliveResult struct {
	Checked   int      // 扫描到的账号数
	Dropped   int      // 本轮置 need_login 的账号数
	Alerted   int      // 发生降级的渠道数
	Truncated bool     // 因 MaxDropsPerTick 截断（还有账号未处理，需人工确认）
	Skipped   []string // 探测失败的渠道（进告警，不静默跳过）
}

// ErrNoAccountProbe 未接账号池探测（保活不可用）。
var ErrNoAccountProbe = errors.New("migration: 未配置账号池探测（account probe）")

// KeepAlive 是会话保活巡检器：探测 / 降级 / 告警三个副作用全部接口注入，
// 于是单测不需要 MySQL / Redis / 浏览器。
type KeepAlive struct {
	store KeepAliveStore
	probe AccountProbe
	drop  AccountDropper
	alert AlertFunc
	cfg   KeepAliveConfig
	now   func() time.Time
}

// NewKeepAlive 构造保活巡检器（store/alert 可为 nil = 对应副作用跳过；probe/drop 必填）。
func NewKeepAlive(store KeepAliveStore, probe AccountProbe, drop AccountDropper, alert AlertFunc, cfg KeepAliveConfig) *KeepAlive {
	if len(cfg.Channels) == 0 {
		cfg.Channels = []string{"x"}
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 72 * time.Hour
	}
	if cfg.MaxDropsPerTick <= 0 {
		cfg.MaxDropsPerTick = 50
	}
	if cfg.Actor == "" {
		cfg.Actor = "keepalive"
	}
	if alert == nil {
		alert = func(context.Context, string, string, string) error { return nil }
	}
	return &KeepAlive{store: store, probe: probe, drop: drop, alert: alert, cfg: cfg, now: time.Now}
}

// Run 跑一轮保活巡检：逐渠道取账号快照 → 判定掉线 → 置 need_login → 汇总告警 + 审计留痕。
func (k *KeepAlive) Run(ctx context.Context) (*KeepAliveResult, error) {
	if k.probe == nil || k.drop == nil {
		return nil, ErrNoAccountProbe
	}
	res := &KeepAliveResult{}
	var alerts []string
	for _, ch := range k.cfg.Channels {
		accounts, err := k.probe(ctx, ch)
		if err != nil {
			// 探测失败必须可见：静默跳过会让「保活没跑」和「跑了但没掉线」长得一模一样。
			res.Skipped = append(res.Skipped, ch)
			alerts = append(alerts, fmt.Sprintf("渠道 %s 账号池探测失败：%v", ch, err))
			continue
		}
		res.Checked += len(accounts)
		var dropped []string
		for _, a := range accounts {
			reason, stale := KeepAliveJudge(a, k.cfg.StaleAfter, k.now())
			if !stale {
				continue
			}
			changed, err := k.drop(ctx, a.Channel, a.AccountID, reason)
			if err != nil {
				return res, fmt.Errorf("置 need_login %s/%s: %w", a.Channel, a.AccountID, err)
			}
			if !changed {
				continue // 已是 need_login：不重复计数、不重复告警（巡检可重复跑）
			}
			dropped = append(dropped, a.AccountID)
			res.Dropped++
			if res.Dropped >= k.cfg.MaxDropsPerTick {
				res.Truncated = true
				break
			}
		}
		if len(dropped) > 0 {
			res.Alerted++
			alerts = append(alerts, fmt.Sprintf("渠道 %s 掉线 %d 个账号（已置 need_login）：%s",
				ch, len(dropped), strings.Join(dropped, ", ")))
			if k.store != nil {
				_ = k.store.Log(ctx, k.cfg.Actor, "session_keepalive_drop", ch, map[string]any{
					"channel": ch, "dropped": dropped, "count": len(dropped),
					"stale_after_sec": int(k.cfg.StaleAfter.Seconds()),
				})
			}
		}
	}
	if len(alerts) > 0 {
		body := strings.Join(alerts, "\n")
		if res.Truncated {
			body += fmt.Sprintf("\n⚠️ 本轮降级已达单轮上限 %d，剩余账号未处理，请人工确认探测链路是否正常。",
				k.cfg.MaxDropsPerTick)
		}
		if err := k.alert(ctx, "会话保活：登录态掉线", body, "warning"); err != nil {
			return res, fmt.Errorf("send keepalive alert: %w", err)
		}
		if k.store != nil {
			_ = k.store.Log(ctx, k.cfg.Actor, "session_keepalive_alert", strings.Join(k.cfg.Channels, ","), map[string]any{
				"checked": res.Checked, "dropped": res.Dropped, "skipped_channels": res.Skipped,
			})
		}
	}
	return res, nil
}

// KeepAliveJudge 判定一个账号的会话是否已失效，返回（原因, 是否失效）。
//
// 规则（只降级、绝不升级）：
//   - banned：人工终态，保活一律不碰（连 need_login 都不写，避免「封号变回待登录」）；
//   - 冷却中且未到 cooldown_until：不动（冷却是限速，不是掉线）；到期后按下面规则判；
//   - 从未成功登录（无 last_login_at）且没有登录态 → 会话必然不可用，判掉线；
//     有登录态但没有回报时间的老数据交给登录回报链定夺（保活不猜）；
//   - 最近一次成功登录距今 > StaleAfter → 会话过期（X 的会话寿命以周计，超期必掉线）。
func KeepAliveJudge(a AccountSnapshot, staleAfter time.Duration, now time.Time) (string, bool) {
	if a.Status == statusBanned {
		return "", false
	}
	if a.Status == statusCooldown && a.CooldownUntil != nil && now.Before(*a.CooldownUntil) {
		return "", false
	}
	if a.LastLoginAt == nil {
		if strings.TrimSpace(a.SessionRef) == "" {
			return "会话保活：从未成功登录且无登录态", true
		}
		return "", false
	}
	if age := now.Sub(*a.LastLoginAt); age > staleAfter {
		return fmt.Sprintf("会话保活：最近登录已过期（%ds > %ds）",
			int(age.Seconds()), int(staleAfter.Seconds())), true
	}
	return "", false
}

// ListSocialAccounts 是 AccountProbe 的默认实现（读 social_accounts，按渠道过滤）。
// 只读；session_ref 只作为「有无登录态」的布尔依据，绝不外传。
func ListSocialAccounts(gdb *gorm.DB) AccountProbe {
	return func(ctx context.Context, channel string) ([]AccountSnapshot, error) {
		var rows []db.SocialAccount
		q := gdb.WithContext(ctx).Order("channel, account_id")
		if channel != "" {
			q = q.Where("channel = ?", channel)
		}
		if err := q.Find(&rows).Error; err != nil {
			return nil, fmt.Errorf("list social_accounts: %w", err)
		}
		out := make([]AccountSnapshot, 0, len(rows))
		for _, r := range rows {
			out = append(out, AccountSnapshot{
				Channel: r.Channel, AccountID: r.AccountID, Status: r.Status,
				SessionRef: r.SessionRef, LastLoginAt: r.LastLoginAt, CooldownUntil: r.CooldownUntil,
			})
		}
		return out, nil
	}
}

// DropSocialAccountNeedLogin 是 AccountDropper 的默认实现：条件 UPDATE 置 need_login。
//
// WHERE 必须同时排除 banned 与 need_login，「changed」才真的等于「发生了状态迁移」：
//   - banned 是人工终态，只能人工解除（与 social 侧 loginreport 同红线）；
//   - 已是 need_login 的账号必须整行不参与：reason 里带着随时间变化的秒数
//     （「最近登录已过期（%ds > %ds）」），MySQL 默认返回**实际变更行数**，只排除 banned
//     时每个 tick 都会改写 last_error 并返回 RowsAffected=1 → changed=true，于是同一个
//     长期掉线的账号会被反复计入 Dropped、反复进告警与审计（Run 里「不重复计数、不重复
//     告警」的约定失效），还会白吃 MaxDropsPerTick 的预算，把真正需要人工看的掉线挤出本轮。
//
// RowsAffected == 0 = 账号已是 need_login / 被封 / 不存在，返回 changed=false。
func DropSocialAccountNeedLogin(gdb *gorm.DB) AccountDropper {
	return func(ctx context.Context, channel, accountID, reason string) (bool, error) {
		res := gdb.WithContext(ctx).Table("social_accounts").
			Where("channel = ? AND account_id = ? AND status NOT IN ?",
				channel, accountID, []string{statusBanned, statusNeedLogin}).
			Updates(map[string]any{"status": statusNeedLogin, "last_error": reason})
		if res.Error != nil {
			return false, fmt.Errorf("update social_accounts: %w", res.Error)
		}
		return res.RowsAffected > 0, nil
	}
}

// EnsureSocialAccountTables 建 social_accounts（幂等：表已存在直接返回 nil）。
//
// 为什么 core 侧要建这张表：social 服务自己会跑内置迁移，但保活巡检（audit）可能与它
// 不在同一批次上线。空库上「探测失败」与「零掉线」必须可区分，所以显式建表，让
// 「还没数据」是可读的零结果而不是报错。列只声明保活要用的那些。
//
// 为什么必须先 HasTable：Migrator().CreateTable 下发的是普通 CREATE TABLE（不判存在），
// MySQL 上表已存在返回 1050。这条路径的后果比建表失败本身更隐蔽 —— cmd/audit 的
// newKeepAlive 一旦返回错误只打一条 WARN，保活巡检从此被**静默关闭**（social 建过表 /
// 本进程第二次启动都会命中）。sqlite 驱动自带 HasTable 守卫，单测看不出这个差异。
func EnsureSocialAccountTables(gdb *gorm.DB) error {
	if gdb.Migrator().HasTable(&db.SocialAccount{}) {
		return nil
	}
	return gdb.Migrator().CreateTable(&db.SocialAccount{})
}
