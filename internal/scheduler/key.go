// Package scheduler 实现上游 Key 池、可用性判定、亲和缓存、选择策略与熔断（spec/07）。
//
// 本包全部在内存中运行，Acquire / Finish 不做任何 I/O（SCH-15）。
package scheduler

import (
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// PersistedState 是需要跨重启保留的 Key 状态（与 store.KeyState* 一一对应）。
type PersistedState int32

const (
	PersistedReady PersistedState = iota
	PersistedQuarantined
	PersistedDisabled
)

// ParsePersistedState 把数据库中的 state 字符串转换为 PersistedState；未知值按 Disabled 处理（最保守）。
func ParsePersistedState(s string) PersistedState {
	switch s {
	case store.KeyStateReady:
		return PersistedReady
	case store.KeyStateQuarantined:
		return PersistedQuarantined
	default:
		return PersistedDisabled
	}
}

func (p PersistedState) String() string {
	switch p {
	case PersistedReady:
		return store.KeyStateReady
	case PersistedQuarantined:
		return store.KeyStateQuarantined
	default:
		return store.KeyStateDisabled
	}
}

// Key 是一个上游 Key 的运行时表示。导出字段在 SetKeys 之后只读。
type Key struct {
	ID          int64
	Pool        string
	Name        string
	Secret      []byte // 解密后的明文，只读；禁止写入日志
	Weight      int
	MaxInFlight int32

	inFlight      atomic.Int32
	persisted     atomic.Int32 // PersistedState
	cooldownUntil atomic.Int64 // UnixNano；0 = 无冷却
	expiresAt     atomic.Int64 // UnixNano；0 = 不过期（T62）
	authFailures  atomic.Int32
	swrrCurrent   int64 // SWRR 当前权重，由池级 mutex 保护
	breaker       breaker
	quota         quotaState // 本期额度用量（T63）
}

// NewKey 构造 Key。maxInFlight < 1 时按 1 处理，weight < 1 时按 1 处理。
func NewKey(id int64, pool, name string, secret []byte, weight, maxInFlight int, state PersistedState) *Key {
	if weight < 1 {
		weight = 1
	}
	if maxInFlight < 1 {
		maxInFlight = 1
	}
	k := &Key{ID: id, Pool: pool, Name: name, Secret: secret, Weight: weight, MaxInFlight: int32(maxInFlight)}
	k.persisted.Store(int32(state))
	return k
}

// InFlight 返回当前在途请求数。
func (k *Key) InFlight() int32 { return k.inFlight.Load() }

// Persisted 返回持久化状态。
func (k *Key) Persisted() PersistedState { return PersistedState(k.persisted.Load()) }

// CooldownUntil 返回冷却截止时间；无冷却返回零值。
func (k *Key) CooldownUntil() time.Time {
	n := k.cooldownUntil.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// SetExpiresAt 设置过期时间，零值表示不过期（T62，SCH-21）。可以在 SetKeys 之前或之后调用。
func (k *Key) SetExpiresAt(t time.Time) {
	if t.IsZero() {
		k.expiresAt.Store(0)
		return
	}
	k.expiresAt.Store(t.UnixNano())
}

// ExpiresAt 返回过期时间；不过期返回零值。
func (k *Key) ExpiresAt() time.Time {
	n := k.expiresAt.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// expired 报告 Key 在 now 时是否已过期（SCH-21）。
func (k *Key) expired(now time.Time) bool {
	n := k.expiresAt.Load()
	return n != 0 && now.UnixNano() >= n
}

// tryAcquire 以 CAS 循环占用一个并发槽位，保证 inFlight 永不超过 MaxInFlight（SCH-14）。
func (k *Key) tryAcquire() bool {
	for {
		cur := k.inFlight.Load()
		if cur >= k.MaxInFlight {
			return false
		}
		if k.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// release 归还一个并发槽位。
func (k *Key) release() {
	k.inFlight.Add(-1)
}

// PoolConfig 是调度器关心的池配置。
type PoolConfig struct {
	Name            string
	Strategy        string // config.StrategyLeastInflight | config.StrategyWeightedRoundRobin
	AffinityTTL     time.Duration
	CooldownDefault time.Duration
	CooldownMax     time.Duration
	Breaker         config.BreakerConfig
	Quota           *Quota // 周期额度（T63）；nil = 不限
}

// PoolsFromConfig 把配置文件中的池转换为 PoolConfig。
func PoolsFromConfig(pools []config.PoolConfig) []PoolConfig {
	out := make([]PoolConfig, 0, len(pools))
	for _, p := range pools {
		out = append(out, PoolConfig{
			Name:            p.Name,
			Strategy:        p.Strategy,
			AffinityTTL:     p.AffinityTTL,
			CooldownDefault: p.Cooldown.Default,
			CooldownMax:     p.Cooldown.Max,
			Breaker:         p.Breaker,
			Quota:           quotaFromConfig(p.Quota),
		})
	}
	return out
}
