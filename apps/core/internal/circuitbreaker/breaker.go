// Package circuitbreaker 是 skill 级熔断器（S3 干预舱，docs/plan/S3-干预舱.md §3.1）：
//
//	close：连续失败 < 5，放行；
//	open ：连续失败 ≥ 5，拒绝（调用方返回 503）；60s 后半开，放行探测请求；
//	half ：探测连续 2 次成功 → close；探测失败 → 回到 open（重置 opened_at）。
//
// 状态持久化到 skill_circuit 表（api 重启不丢状态，S1 建表）。
package circuitbreaker

import (
	"context"
	"sync"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// 状态机参数（S3 §3.1 固定值）
const (
	StateClose = "close"
	StateOpen  = "open"
	StateHalf  = "half"

	failThreshold      = 5                // 连续失败 5 次 → open
	halfOpenAfter      = 60 * time.Second // open 60s 后 → half
	closeSuccessStreak = 2                // half 下连续 2 次成功 → close
)

// circuitRow 对应 skill_circuit 表。
type circuitRow struct {
	SkillName           string     `gorm:"column:skill_name;primaryKey" json:"skill_name"`
	State               string     `gorm:"column:state;not null;default:close" json:"state"`
	ConsecutiveFailures int        `gorm:"column:consecutive_failures;not null;default:0" json:"consecutive_failures"`
	OpenedAt            *time.Time `gorm:"column:opened_at" json:"opened_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

func (circuitRow) TableName() string { return "skill_circuit" }

// Breaker 是单 skill 的熔断器。线程安全；状态变更持久化到 skill_circuit。
type Breaker struct {
	mu                  sync.Mutex
	name                string
	state               string
	consecutiveFailures int
	openedAt            time.Time
	successStreak       int // half 状态下的连续成功计数

	database *db.DB
	now      func() time.Time
}

// NewBreaker 创建熔断器并从 DB 恢复状态（无记录时默认 close）。
func NewBreaker(database *db.DB, name string) *Breaker {
	b := &Breaker{
		name:     name,
		state:    StateClose,
		database: database,
		now:      time.Now,
	}
	b.load()
	return b
}

// load 从 skill_circuit 恢复状态（表不存在/无记录时保持 close）。
func (b *Breaker) load() {
	if b.database == nil {
		return
	}
	var row circuitRow
	if err := b.database.Where("skill_name = ?", b.name).First(&row).Error; err != nil {
		return // 无记录（或表缺失）：默认 close
	}
	b.state = row.State
	b.consecutiveFailures = row.ConsecutiveFailures
	if row.OpenedAt != nil {
		b.openedAt = *row.OpenedAt
	}
}

// persist 把当前状态写入 skill_circuit（upsert）。
func (b *Breaker) persist(ctx context.Context) {
	if b.database == nil {
		return
	}
	var openedAt *time.Time
	if b.state == StateOpen || b.state == StateHalf {
		openedAt = &b.openedAt
	}
	row := circuitRow{
		SkillName:           b.name,
		State:               b.state,
		ConsecutiveFailures: b.consecutiveFailures,
		OpenedAt:            openedAt,
		UpdatedAt:           b.now(),
	}
	if err := b.database.WithContext(ctx).Save(&row).Error; err != nil {
		// 持久化失败不阻断调用链：内存状态仍是运行期真相
		_ = err
	}
}

// Allow 判断本次调用是否放行。open 且未到半开窗口 → false（调用方 503）。
func (b *Breaker) Allow(ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClose:
		return true
	case StateOpen:
		if b.now().Sub(b.openedAt) >= halfOpenAfter {
			b.state = StateHalf
			b.successStreak = 0
			b.persist(ctx)
			return true // 放行一个探测请求
		}
		return false
	case StateHalf:
		return true
	default:
		return true
	}
}

// RecordSuccess 记录一次成功调用。half 下连续 2 次成功 → close。
func (b *Breaker) RecordSuccess(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateHalf:
		b.successStreak++
		if b.successStreak >= closeSuccessStreak {
			b.state = StateClose
			b.consecutiveFailures = 0
			b.successStreak = 0
			b.persist(ctx)
		}
	case StateClose:
		b.consecutiveFailures = 0
	}
}

// RecordFailure 记录一次失败调用。返回 true 表示本次失败触发了 open（调用方应写 intervention + 告警）。
func (b *Breaker) RecordFailure(ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateHalf:
		b.state = StateOpen
		b.openedAt = b.now()
		b.consecutiveFailures = failThreshold
		b.successStreak = 0
		b.persist(ctx)
		return true
	case StateClose:
		b.consecutiveFailures++
		if b.consecutiveFailures >= failThreshold {
			b.state = StateOpen
			b.openedAt = b.now()
			b.persist(ctx)
			return true
		}
	}
	return false
}

// ForceOpen 强制 open（灰度联动：S1 gray.go 窗口内失败率恶化时调用，S3 §3.1）。
func (b *Breaker) ForceOpen(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen {
		return
	}
	b.state = StateOpen
	b.openedAt = b.now()
	b.consecutiveFailures = failThreshold
	b.successStreak = 0
	b.persist(ctx)
}

// Reset 重置为 close（干预 retry 用，S3 §3.3）。
func (b *Breaker) Reset(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClose
	b.consecutiveFailures = 0
	b.successStreak = 0
	b.openedAt = time.Time{}
	b.persist(ctx)
}

// State 返回当前状态（close/open/half）。
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Name 返回 skill 名。
func (b *Breaker) Name() string { return b.name }

// Registry 按 skill 名懒加载熔断器实例（每个 skill 一个）。
type Registry struct {
	mu       sync.RWMutex
	database *db.DB
	breakers map[string]*Breaker
}

// NewRegistry 创建熔断器注册表。
func NewRegistry(database *db.DB) *Registry {
	return &Registry{database: database, breakers: map[string]*Breaker{}}
}

// Get 返回某 skill 的熔断器（不存在时创建并从 DB 恢复状态）。
func (r *Registry) Get(name string) *Breaker {
	r.mu.RLock()
	b, ok := r.breakers[name]
	r.mu.RUnlock()
	if ok {
		return b
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok = r.breakers[name]; ok {
		return b
	}
	b = NewBreaker(r.database, name)
	r.breakers[name] = b
	return b
}

// All 返回全部已知熔断器的状态（board 断路器圆点用）。
func (r *Registry) All() []circuitRow {
	r.mu.RLock()
	names := make([]string, 0, len(r.breakers))
	for n := range r.breakers {
		names = append(names, n)
	}
	r.mu.RUnlock()
	var rows []circuitRow
	if r.database == nil {
		return rows
	}
	if len(names) > 0 {
		_ = r.database.Where("skill_name IN ?", names).Find(&rows).Error
	} else {
		_ = r.database.Find(&rows).Error
	}
	return rows
}
