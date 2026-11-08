package scheduler

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
)

// Options 是调度器的可选依赖。
type Options struct {
	Now              func() time.Time // 为 nil 时用 time.Now
	Logger           *slog.Logger
	AffinityCapacity int // 0 = 默认 65536
	// OnQuarantine 在 Key 因连续鉴权失败被隔离时调用（ST-06）。
	// 调度器必须在不持有任何锁的情况下调用它；回调中可以做 I/O（例如写库）。
	OnQuarantine func(keyID int64, reason string)
}

// AcquireRequest 是一次选择请求。
type AcquireRequest struct {
	Pool        string
	AffinityKey string  // 空 = 无亲和
	Exclude     []int64 // 本请求已尝试失败的 Key
}

// Lease 是对一个 Key 的占用。必须且只需调用一次 Finish。
type Lease struct {
	Key *Key

	s        *Scheduler
	pool     *pool
	probe    bool
	finished atomic.Bool
}

// KeyStatus 是管理 API 展示用的 Key 状态。
type KeyStatus struct {
	ID                int64
	Pool              string
	Name              string
	Persisted         PersistedState
	InFlight          int
	MaxInFlight       int
	CooldownRemaining time.Duration
	Breaker           BreakerState
	AuthFailures      int
}

// pool 是一个池的运行时状态。
type pool struct {
	cfg    PoolConfig
	mu     sync.Mutex // 保护 keys 切片替换与 SWRR 运算
	keys   []*Key     // guarded by mu
	cursor atomic.Uint64
}

// Scheduler 管理全部池。并发安全。
type Scheduler struct {
	opts     Options
	pools    map[string]*pool // 构造后只读
	affinity *affinityCache
}

// New 创建调度器。pools 中的名称必须唯一（由 config.Validate 保证）。
func New(pools []PoolConfig, opts Options) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Scheduler{opts: opts, pools: map[string]*pool{}, affinity: newAffinityCache(opts.AffinityCapacity)}
	for _, pc := range pools {
		s.pools[pc.Name] = &pool{cfg: pc}
	}
	return s
}

// SetKeys 用 keys 替换全部 Key，同 ID 的处理规则见 SCH-16。Pool 不存在的 Key 被忽略并记录 Warn。
func (s *Scheduler) SetKeys(keys []*Key) {
	// TODO(T08)
}

// Acquire 按 spec/07 §3 选择一个 Key。无可用 Key 返回 CodeUpstreamUnavailable（§3.1）。
func (s *Scheduler) Acquire(req AcquireRequest) (*Lease, error) {
	// TODO(T08): SCH-01 ~ SCH-15。
	return nil, apperr.NotImplemented("T08")
}

// Finish 释放占用并按 Outcome 更新 Key 状态（spec/07 §5，ST-01 ~ ST-09）。重复调用无效。
func (l *Lease) Finish(o provider.Outcome) {
	// TODO(T08)
}

// KeyStatus 返回全部 Key 的状态快照，按 ID 升序。
func (s *Scheduler) KeyStatus() []KeyStatus {
	// TODO(T08)
	return nil
}
