// config.go：M4-1 三个组件（保活 / 切流 / 下线）的环境变量装配。
//
// 约定与全系统一致：运行时配置全走环境变量（.env），失败由调用方决定 fatal 还是降级。
package migration

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// EnvConfig 是 M4-1 的运行时配置（三个组件共用一份）。
type EnvConfig struct {
	// 会话保活：巡检覆盖的渠道（逗号分隔）、会话过期阈值、单轮降级上限。
	KeepAliveChannels string `env:"KEEPALIVE_CHANNELS" envDefault:"x"`
	KeepAliveStaleSec int    `env:"KEEPALIVE_STALE_SEC" envDefault:"259200"` // 3 天
	KeepAliveMaxDrops int    `env:"KEEPALIVE_MAX_DROPS_PER_TICK" envDefault:"50"`

	// 切流护栏（缺省阈值；skill_cutover 行上的阈值优先）。
	CutoverMinStageSec int     `env:"CUTOVER_MIN_STAGE_SEC" envDefault:"1800"` // 30 分钟
	CutoverMinSamples  int     `env:"CUTOVER_MIN_SAMPLES" envDefault:"20"`
	CutoverParityMin   float64 `env:"CUTOVER_PARITY_MIN" envDefault:"0.9"`
	CutoverSuccessMin  float64 `env:"CUTOVER_SUCCESS_MIN" envDefault:"0.95"`

	// 旧服务下线：100% 档的稳定观察窗。
	DecommissionStableSec int `env:"DECOMMISSION_STABLE_SEC" envDefault:"86400"` // 24h
}

// LoadEnvConfig 解析 EnvConfig（缺省值齐全，不会因缺变量而失败）。
func LoadEnvConfig() (*EnvConfig, error) {
	cfg := &EnvConfig{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("migration: load env: %w", err)
	}
	return cfg, nil
}

// KeepAliveConfig 转成保活巡检配置。
func (c *EnvConfig) KeepAliveConfig() KeepAliveConfig {
	return KeepAliveConfig{
		Channels:        SplitList(c.KeepAliveChannels),
		StaleAfter:      time.Duration(c.KeepAliveStaleSec) * time.Second,
		MaxDropsPerTick: c.KeepAliveMaxDrops,
		Actor:           "keepalive",
	}
}

// CutoverConfig 转成切流护栏配置（Actor 固定 cutover，阈值经 WithDefaults 补齐）。
//
// ⚠️ 这是 .env 里 CUTOVER_* 四个阈值进入运行期的唯一入口：装配切流服务的地方（cmd/api）
// 必须用它构造 CutoverService，不要就地写死 CutoverConfig{...} —— 阈值写死之后，运维按
// .env.example 收紧放量门槛会完全不生效（护栏照旧放行），且这份写死的 cfg 还会经
// CutoverService.Config() 喂给 skill-decommission/plan，两个端点一起偏离环境配置。
func (c *EnvConfig) CutoverConfig() CutoverConfig {
	return CutoverConfig{
		Actor:            "cutover",
		MinStageInterval: time.Duration(c.CutoverMinStageSec) * time.Second,
		MinSamples:       c.CutoverMinSamples,
		ParityMin:        c.CutoverParityMin,
		SuccessMin:       c.CutoverSuccessMin,
	}.WithDefaults()
}

// DecommissionWindow 返回旧服务下线的稳定观察窗。
func (c *EnvConfig) DecommissionWindow() time.Duration {
	if c.DecommissionStableSec <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(c.DecommissionStableSec) * time.Second
}

// SplitList 解析逗号分隔清单（去空白、去空项、保序）。
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
