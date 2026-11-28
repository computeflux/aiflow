// M4-1（工作项 wi-1795489658-2d59）会话保活单测：掉线判定 + 一轮巡检的行为。
//
// 覆盖验收标准「定时心跳检测掉线置 need_login + 告警」的行为点：
//   - 会话过期 / 从未登录且无登录态 → 置 need_login，并写审计 + 发告警；
//   - 最近登录未过期 / 冷却未到期 → 不动；
//   - banned 是人工终态：保活绝不改（连 need_login 都不写）；
//   - need_login 账号不重复计数（幂等：第二轮巡检零变更零告警）；
//   - 单轮降级上限（探测链路故障时不把整池账号打空）与截断标记；
//   - 渠道探测失败必须进告警（不静默跳过）。
//
// CI 安全：MySQL / Redis / 浏览器一个都不起 —— 探测、降级、告警三个副作用全是内存假实现。
package migration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type logEntry struct {
	actor, action, target string
	detail                map[string]any
}

type fakeStore struct{ logs []logEntry }

func (f *fakeStore) Log(_ context.Context, actor, action, target string, detail any) error {
	d, _ := detail.(map[string]any)
	f.logs = append(f.logs, logEntry{actor, action, target, d})
	return nil
}

type fakeAlert struct {
	calls []notifyAlert
	err   error
}

type notifyAlert struct{ title, body, level string }

func (f *fakeAlert) send(_ context.Context, title, body, level string) error {
	f.calls = append(f.calls, notifyAlert{title, body, level})
	return f.err
}

// fakePool 是账号池探测 + 降级的内存实现（记录全部降级调用）。
type fakePool struct {
	accounts map[string][]accountRow
	dropped  []string // "channel/account"
	dropErr  error
	probeErr map[string]error
}

type accountRow struct {
	channel, id, status, sessionRef string
	lastLogin                       *time.Time
	cooldownUntil                   *time.Time
}

func newPool(rows ...accountRow) *fakePool {
	p := &fakePool{accounts: map[string][]accountRow{}, probeErr: map[string]error{}}
	for _, r := range rows {
		p.accounts[r.channel] = append(p.accounts[r.channel], r)
	}
	return p
}

func (p *fakePool) probe(_ context.Context, channel string) ([]AccountSnapshot, error) {
	if err := p.probeErr[channel]; err != nil {
		return nil, err
	}
	var out []AccountSnapshot
	for _, a := range p.accounts[channel] {
		out = append(out, AccountSnapshot{Channel: a.channel, AccountID: a.id, Status: a.status,
			SessionRef: a.sessionRef, LastLoginAt: a.lastLogin, CooldownUntil: a.cooldownUntil})
	}
	return out, nil
}

func (p *fakePool) drop(_ context.Context, channel, accountID, reason string) (bool, error) {
	if p.dropErr != nil {
		return false, p.dropErr
	}
	for i, a := range p.accounts[channel] {
		if a.id != accountID {
			continue
		}
		if a.status == statusBanned || a.status == statusNeedLogin {
			return false, nil // 已是终态/待登录：无变更
		}
		p.accounts[channel][i].status = statusNeedLogin
		p.dropped = append(p.dropped, channel+"/"+accountID)
		_ = reason
		return true, nil
	}
	return false, nil
}

func ts(t time.Time) *time.Time { return &t }

func statusOf(p *fakePool, channel, id string) string {
	for _, a := range p.accounts[channel] {
		if a.id == id {
			return a.status
		}
	}
	return ""
}

func TestKeepAliveJudge(t *testing.T) {
	now := time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	stale := now.Add(-100 * time.Hour)

	cases := []struct {
		name    string
		acc     AccountSnapshot
		staleAt time.Duration
		want    bool
	}{
		{"最近登录未过期不动", AccountSnapshot{Status: statusActive, SessionRef: "v1.x", LastLoginAt: ts(fresh)}, 72 * time.Hour, false},
		{"登录超过阈值判掉线", AccountSnapshot{Status: statusActive, SessionRef: "v1.x", LastLoginAt: ts(stale)}, 72 * time.Hour, true},
		{"从未登录且无登录态判掉线", AccountSnapshot{Status: statusActive, SessionRef: ""}, 72 * time.Hour, true},
		{"有登录态但无回报时间不动（老数据交给登录回报链）", AccountSnapshot{Status: statusActive, SessionRef: "v1.x"}, 72 * time.Hour, false},
		{"冷却未到期不动（冷却是限速不是掉线）", AccountSnapshot{Status: statusCooldown, SessionRef: "v1.x", LastLoginAt: ts(stale), CooldownUntil: ts(now.Add(5 * time.Minute))}, 72 * time.Hour, false},
		{"冷却已到期的过期账号判掉线", AccountSnapshot{Status: statusCooldown, SessionRef: "v1.x", LastLoginAt: ts(stale), CooldownUntil: ts(now.Add(-time.Minute))}, 72 * time.Hour, true},
		{"banned 绝不参与保活", AccountSnapshot{Status: statusBanned, SessionRef: "v1.x", LastLoginAt: ts(stale)}, 72 * time.Hour, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, got := KeepAliveJudge(c.acc, c.staleAt, now)
			if got != c.want {
				t.Fatalf("KeepAliveJudge = %v (reason %q), want %v", got, reason, c.want)
			}
			if got && strings.TrimSpace(reason) == "" {
				t.Fatal("掉线必须带原因（写进 last_error 供 board 展示）")
			}
			if !got && reason != "" {
				t.Fatalf("未掉线不应带原因，得到 %q", reason)
			}
		})
	}
}

func TestKeepAliveRunDropsStaleAccounts(t *testing.T) {
	now := time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * time.Hour)
	pool := newPool(
		accountRow{channel: "x", id: "bot-a", status: statusActive, sessionRef: "v1.aa", lastLogin: ts(stale)},
		accountRow{channel: "x", id: "bot-b", status: statusActive, sessionRef: "v1.bb", lastLogin: ts(stale)},
		accountRow{channel: "x", id: "bot-ok", status: statusActive, sessionRef: "v1.cc", lastLogin: ts(now.Add(-time.Hour))},
		accountRow{channel: "x", id: "bot-banned", status: statusBanned, sessionRef: "v1.dd", lastLogin: ts(stale)},
	)
	st := &fakeStore{}
	al := &fakeAlert{}
	k := NewKeepAlive(st, pool.probe, pool.drop, al.send, KeepAliveConfig{Channels: []string{"x"}, StaleAfter: 72 * time.Hour})
	k.now = func() time.Time { return now }

	res, err := k.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checked != 4 || res.Dropped != 2 {
		t.Fatalf("checked=%d dropped=%d, want 4/2", res.Checked, res.Dropped)
	}
	if got := statusOf(pool, "x", "bot-a"); got != statusNeedLogin {
		t.Fatalf("掉线账号 bot-a 应置 need_login，得到 %q", got)
	}
	if got := statusOf(pool, "x", "bot-banned"); got != statusBanned {
		t.Fatalf("banned 账号必须保持 banned，得到 %q", got)
	}
	if got := statusOf(pool, "x", "bot-ok"); got != statusActive {
		t.Fatalf("未过期账号不该被动到，得到 %q", got)
	}
	if len(al.calls) != 1 {
		t.Fatalf("应发一条告警，得到 %d 条", len(al.calls))
	}
	if al.calls[0].level != "warning" || !strings.Contains(al.calls[0].body, "bot-a") || !strings.Contains(al.calls[0].body, "bot-b") {
		t.Fatalf("告警内容异常: %+v", al.calls[0])
	}
	if strings.Contains(al.calls[0].body, "v1.aa") {
		t.Fatal("告警绝不允许出现登录态（session_ref）")
	}
	var dropLogs, alertLogs int
	for _, l := range st.logs {
		switch l.action {
		case "session_keepalive_drop":
			dropLogs++
			if l.actor != "keepalive" {
				t.Fatalf("审计 actor = %q, want keepalive", l.actor)
			}
			if got := l.detail["stale_after_sec"]; got != int((72 * time.Hour).Seconds()) {
				t.Fatalf("审计缺阈值: %+v", l.detail)
			}
		case "session_keepalive_alert":
			alertLogs++
		}
	}
	if dropLogs != 1 || alertLogs != 1 {
		t.Fatalf("审计留痕异常 drop=%d alert=%d（全部日志 %+v）", dropLogs, alertLogs, st.logs)
	}
}

func TestKeepAliveRunIsIdempotent(t *testing.T) {
	now := time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * time.Hour)
	pool := newPool(accountRow{channel: "x", id: "bot-a", status: statusActive, sessionRef: "v1.aa", lastLogin: ts(stale)})
	al := &fakeAlert{}
	k := NewKeepAlive(&fakeStore{}, pool.probe, pool.drop, al.send, KeepAliveConfig{Channels: []string{"x"}})
	k.now = func() time.Time { return now }

	if _, err := k.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	res, err := k.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Dropped != 0 || res.Alerted != 0 {
		t.Fatalf("第二轮应零变更零告警（幂等），得到 %+v", res)
	}
	if len(al.calls) != 1 {
		t.Fatalf("告警只该在真正变更时发一次，得到 %d 条", len(al.calls))
	}
}

func TestKeepAliveRunCapsDropsPerTick(t *testing.T) {
	now := time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * time.Hour)
	var rows []accountRow
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		rows = append(rows, accountRow{channel: "x", id: id, status: statusActive, sessionRef: "v1." + id, lastLogin: ts(stale)})
	}
	pool := newPool(rows...)
	al := &fakeAlert{}
	k := NewKeepAlive(&fakeStore{}, pool.probe, pool.drop, al.send, KeepAliveConfig{Channels: []string{"x"}, MaxDropsPerTick: 2})
	k.now = func() time.Time { return now }

	res, err := k.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Dropped != 2 || !res.Truncated {
		t.Fatalf("应只降级 2 个并标记截断，得到 %+v", res)
	}
	if !strings.Contains(al.calls[0].body, "上限") {
		t.Fatalf("截断必须在告警里说清楚（防探测故障把整池打空）：%s", al.calls[0].body)
	}
}

func TestKeepAliveRunReportsProbeFailure(t *testing.T) {
	pool := newPool()
	pool.probeErr["x"] = errors.New("dial tcp: connect refused")
	st := &fakeStore{}
	al := &fakeAlert{}
	k := NewKeepAlive(st, pool.probe, pool.drop, al.send, KeepAliveConfig{Channels: []string{"x"}})

	res, err := k.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "x" {
		t.Fatalf("探测失败的渠道必须记进 Skipped（不静默）：%+v", res)
	}
	if len(al.calls) != 1 || !strings.Contains(al.calls[0].body, "探测失败") {
		t.Fatalf("探测失败必须告警：%+v", al.calls)
	}
}

func TestKeepAliveRunPropagatesDropAndAlertErrors(t *testing.T) {
	now := time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * time.Hour)
	pool := newPool(accountRow{channel: "x", id: "bot-a", status: statusActive, sessionRef: "v1.aa", lastLogin: ts(stale)})
	k := NewKeepAlive(&fakeStore{}, pool.probe, pool.drop, (&fakeAlert{}).send, KeepAliveConfig{Channels: []string{"x"}})
	k.now = func() time.Time { return now }
	pool.dropErr = errors.New("db down")
	if _, err := k.Run(context.Background()); err == nil {
		t.Fatal("降级写库失败必须报错（不能当成「保活成功」）")
	}

	pool2 := newPool(accountRow{channel: "x", id: "bot-a", status: statusActive, sessionRef: "v1.aa", lastLogin: ts(stale)})
	k2 := NewKeepAlive(&fakeStore{}, pool2.probe, pool2.drop, (&fakeAlert{err: errors.New("matrix down")}).send, KeepAliveConfig{Channels: []string{"x"}})
	k2.now = func() time.Time { return now }
	if _, err := k2.Run(context.Background()); err == nil {
		t.Fatal("告警发送失败必须报错（掉线没人知道 = 保活没起作用）")
	}
}

func TestKeepAliveRequiresProbe(t *testing.T) {
	k := NewKeepAlive(&fakeStore{}, nil, nil, nil, KeepAliveConfig{})
	if _, err := k.Run(context.Background()); !errors.Is(err, ErrNoAccountProbe) {
		t.Fatalf("未接账号池应报 ErrNoAccountProbe，得到 %v", err)
	}
	if k.cfg.StaleAfter <= 0 || k.cfg.MaxDropsPerTick <= 0 || len(k.cfg.Channels) == 0 || k.cfg.Actor == "" {
		t.Fatalf("缺省值未填充: %+v", k.cfg)
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList(" x , ,bili,")
	if len(got) != 2 || got[0] != "x" || got[1] != "bili" {
		t.Fatalf("SplitList = %v, want [x bili]", got)
	}
	if len(SplitList("")) != 0 {
		t.Fatal("空串应得空清单")
	}
}

func TestEnvConfigDefaults(t *testing.T) {
	cfg, err := LoadEnvConfig()
	if err != nil {
		t.Fatalf("LoadEnvConfig: %v", err)
	}
	ka := cfg.KeepAliveConfig()
	if len(ka.Channels) != 1 || ka.Channels[0] != "x" {
		t.Fatalf("缺省渠道 = %v, want [x]", ka.Channels)
	}
	if ka.StaleAfter <= 0 || ka.MaxDropsPerTick <= 0 {
		t.Fatalf("保活缺省值异常: %+v", ka)
	}
	ct := cfg.CutoverConfig()
	if ct.MinStageInterval <= 0 || ct.MinSamples <= 0 || ct.ParityMin <= 0 || ct.SuccessMin <= 0 {
		t.Fatalf("切流缺省阈值异常: %+v", ct)
	}
	if ct.SuccessMin < ct.ParityMin {
		t.Fatalf("成功率阈值应严于一致率（成功率是硬指标）: %+v", ct)
	}
	if cfg.DecommissionWindow() <= 0 {
		t.Fatal("下线观察窗缺省值异常")
	}
}
