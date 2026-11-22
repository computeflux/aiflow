package skillgateway

import (
	"errors"
	"sync"

	"golang.org/x/time/rate"
)

// ErrRateLimited：超过 rate_per_min 限制。
var ErrRateLimited = errors.New("skill rate limited")

// ErrConcurrencyLimited：超过 max_concurrency 限制。
var ErrConcurrencyLimited = errors.New("skill concurrency limited")

// limitEntry 是一个 skill 的限流器与并发信号量。
type limitEntry struct {
	limiter *rate.Limiter // nil 表示不限流
	sem     chan struct{} // nil 表示不限并发
}

// limiterStore 按 skill 名维护限流状态（进程内）。
type limiterStore struct {
	mu    sync.Mutex
	items map[string]*limitEntry
}

func newLimiterStore() *limiterStore {
	return &limiterStore{items: make(map[string]*limitEntry)}
}

// entry 返回（必要时创建）skill 的限流条目。
func (s *limiterStore) entry(name string, l Limits) *limitEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.items[name]; ok {
		return e
	}
	e := &limitEntry{}
	if l.RatePerMin > 0 {
		// burst=1：保守——同一 skill 的并发请求最多 1 个先通过，
		// 其余立即 429，便于 DoD 验证与客户端重试。
		e.limiter = rate.NewLimiter(rate.Limit(float64(l.RatePerMin)/60.0), 1)
	}
	if l.MaxConcurrency > 0 {
		e.sem = make(chan struct{}, l.MaxConcurrency)
	}
	s.items[name] = e
	return e
}

// acquire 尝试获取执行许可；失败返回 ErrRateLimited / ErrConcurrencyLimited。
// 成功时返回的 release 必须调用（defer）。
func (e *limitEntry) acquire() (release func(), err error) {
	if e.limiter != nil && !e.limiter.Allow() {
		return nil, ErrRateLimited
	}
	if e.sem != nil {
		select {
		case e.sem <- struct{}{}:
		default:
			return nil, ErrConcurrencyLimited
		}
		return func() { <-e.sem }, nil
	}
	return func() {}, nil
}
