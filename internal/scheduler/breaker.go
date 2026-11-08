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
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.currentLocked(now) {
	case BreakerClosed:
		return true
	case BreakerHalfOpen:
		return !b.probing
	default:
		return false
	}
}

// onAcquire 在 Key 被选中（已占用并发槽位）后调用，原子地确认放行：
// closed → (false, true)；half_open 且无探测 → 标记探测进行中，(true, true)；
// 其他情况（另一个请求抢先成为探测、或已重新打开）→ (false, false)，调用方必须归还槽位并换 Key。
func (b *breaker) onAcquire(now time.Time) (probe, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.currentLocked(now) {
	case BreakerClosed:
		return false, true
	case BreakerHalfOpen:
		if b.probing {
			return false, false
		}
		b.probing = true
		return true, true
	default:
		return false, false
	}
}

// record 记录一次结果：success=true 计成功（ST-01），false 计失败（ST-09）。probe 表示该请求是否是探测请求。
func (b *breaker) record(success, probe bool, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
		if success { // 探测成功 → closed，清空窗口，d 重置
			b.state = BreakerClosed
			b.buckets = [breakerBuckets]breakerBucket{}
			b.openFor = b.cfg.OpenDuration
		} else { // 探测失败 → open，d 翻倍且不超过上限
			b.openFor = min(b.openFor*2, b.cfg.MaxOpenDuration)
			b.trip(now)
		}
		return
	}
	if b.state != BreakerClosed || b.cfg.Window <= 0 {
		return // 打开期间结束的旧请求不影响状态
	}
	width := b.cfg.Window / breakerBuckets
	if width <= 0 {
		width = time.Nanosecond
	}
	start := now.Truncate(width)
	bk := &b.buckets[(start.UnixNano()/int64(width))%breakerBuckets]
	if !bk.start.Equal(start) {
		*bk = breakerBucket{start: start}
	}
	bk.total++
	if !success {
		bk.failures++
	}
	total, failures := 0, 0
	horizon := now.Add(-b.cfg.Window)
	for i := range b.buckets {
		if !b.buckets[i].start.Before(horizon) {
			total += b.buckets[i].total
			failures += b.buckets[i].failures
		}
	}
	if total >= b.cfg.MinRequests && total > 0 && float64(failures)/float64(total) >= b.cfg.FailureRatio {
		b.trip(now)
	}
}

// trip 进入 open 状态。调用方持有 mu。
func (b *breaker) trip(now time.Time) {
	b.state = BreakerOpen
	b.openedAt = now
	if b.openFor <= 0 {
		b.openFor = b.cfg.OpenDuration
	}
}

// cancelProbe 在探测请求以「不计入统计」的 Outcome 结束时调用，释放探测名额。
func (b *breaker) cancelProbe() {
	b.mu.Lock()
	b.probing = false
	b.mu.Unlock()
}

// current 返回当前状态（会触发 open → half_open 的惰性转换）。
func (b *breaker) current(now time.Time) BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentLocked(now)
}

func (b *breaker) currentLocked(now time.Time) BreakerState {
	if b.state == BreakerOpen && !now.Before(b.openedAt.Add(b.openFor)) {
		b.state = BreakerHalfOpen
		b.probing = false
	}
	return b.state
}
