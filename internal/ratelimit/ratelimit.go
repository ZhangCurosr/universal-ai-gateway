// Package ratelimit 实现每客户端 RPM 令牌桶（spec/09 §3，RL-01 ~ RL-06）。
package ratelimit

import (
	"sync"
	"time"
)

// Limiter 为每个客户端维护一个令牌桶。并发安全。
type Limiter struct {
	now     func() time.Time
	buckets sync.Map // int64 → *bucket
}

// bucket 是单个客户端的令牌桶。
type bucket struct {
	mu       sync.Mutex
	rpm      int       // guarded by mu
	tokens   float64   // guarded by mu
	last     time.Time // guarded by mu：上次补充令牌的时间
	lastSeen time.Time // guarded by mu：上次访问时间（Sweep 用）
}

// New 创建限流器。now 为 nil 时使用 time.Now。
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now}
}

// Allow 尝试为 clientID 消耗 1 个令牌。拒绝时返回需要等待的时长。
func (l *Limiter) Allow(clientID int64, rpm int) (ok bool, retryAfter time.Duration) {
	// TODO(T04): 实现 RL-01 ~ RL-05。
	return true, 0
}

// Sweep 删除 now 之前 10 分钟内未访问的桶（RL-06）。
func (l *Limiter) Sweep(now time.Time) {
	// TODO(T04): 实现 RL-06。
}
