package scheduler

import (
	"sync"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// BreakerState 是熔断器状态（spec/07 §5.1）。
type BreakerState int

const (
	BreakerClosed BreakerState = iota
	BreakerOpen
	BreakerHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

const breakerBuckets = 10

// breaker 是单个 Key 的滑动窗口熔断器。所有方法接收 now 参数，自己不取时间。
type breaker struct {
	mu       sync.Mutex
	cfg      config.BreakerConfig // guarded by mu
	state    BreakerState         // guarded by mu
	buckets  [breakerBuckets]breakerBucket
	openedAt time.Time     // guarded by mu
	openFor  time.Duration // guarded by mu：当前打开时长 d
	probing  bool          // guarded by mu：half_open 时是否有探测请求进行中
}

type breakerBucket struct {
	start    time.Time // 桶起始时间（按 window/10 对齐）
	total    int
	failures int
}

// configure 设置配置（SetKeys 时调用）。已有统计保留。
func (b *breaker) configure(cfg config.BreakerConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg
	if b.openFor == 0 {
		b.openFor = cfg.OpenDuration
	}
}

// allow 报告此刻是否允许放行一个请求（SCH-04）。open 到期时惰性转为 half_open。
func (b *breaker) allow(now time.Time) bool {
	// TODO(T08)
	return true
}

// onAcquire 在 Key 被选中后调用；若处于 half_open 则标记探测进行中并返回 true。
func (b *breaker) onAcquire(now time.Time) (probe bool) {
	// TODO(T08)
	return false
}

// record 记录一次结果：success=true 计成功（ST-01），false 计失败（ST-09）。probe 表示该请求是否是探测请求。
func (b *breaker) record(success, probe bool, now time.Time) {
	// TODO(T08)
}

// cancelProbe 在探测请求以「不计入统计」的 Outcome 结束时调用，释放探测名额。
func (b *breaker) cancelProbe() {
	// TODO(T08)
}

// current 返回当前状态（会触发 open → half_open 的惰性转换）。
func (b *breaker) current(now time.Time) BreakerState {
	// TODO(T08)
	return BreakerClosed
}
