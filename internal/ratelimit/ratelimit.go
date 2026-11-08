// Package ratelimit 实现按键的 RPM 令牌桶（spec/09 §3，RL-01 ~ RL-06）。
// 同一实现既用于每客户端限流（键 "c:<clientID>"），也用于鉴权前的每 IP 限流（键 "ip:<addr>"）。
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter 为每个键维护一个令牌桶。并发安全。
type Limiter struct {
	now     func() time.Time
	buckets sync.Map // string → *bucket
}

// bucket 是单个客户端的令牌桶。
type bucket struct {
	mu       sync.Mutex
	rpm      int       // guarded by mu
	tokens   float64   // guarded by mu
	last     time.Time // guarded by mu：上次补充令牌的时间
	lastSeen time.Time // guarded by mu：上次访问时间（Sweep 用）
	deleted  bool      // guarded by mu：标记已被 Sweep 删除
}

// New 创建限流器。now 为 nil 时使用 time.Now。
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now}
}

func (l *Limiter) timeNow() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Allow 尝试为 key 消耗 1 个令牌（RL-01 ~ RL-05）。拒绝时返回需要等待的时长。
func (l *Limiter) Allow(key string, rpm int) (ok bool, retryAfter time.Duration) {
	// RL-01: rpm <= 0 永远允许，retryAfter = 0
	if rpm <= 0 {
		return true, 0
	}

	for {
		val, ok := l.buckets.Load(key)
		var b *bucket
		if ok {
			b = val.(*bucket)
		} else {
			now := l.timeNow()
			newB := &bucket{
				rpm:      rpm,
				tokens:   float64(rpm),
				last:     now,
				lastSeen: now,
			}
			actual, _ := l.buckets.LoadOrStore(key, newB)
			b = actual.(*bucket)
		}

		b.mu.Lock()
		if b.deleted {
			b.mu.Unlock()
			continue
		}

		now := l.timeNow()
		b.lastSeen = now

		// 补充令牌（RL-02）
		if delta := now.Sub(b.last); delta > 0 {
			refillRate := float64(b.rpm) / 60.0
			b.tokens += delta.Seconds() * refillRate
			if b.tokens > float64(b.rpm) {
				b.tokens = float64(b.rpm)
			}
			b.last = now
		} else if delta < 0 {
			// 时钟回拨时校准 last
			b.last = now
		}

		// rpm 改变时（RL-04）
		if b.rpm != rpm {
			b.rpm = rpm
			if b.tokens > float64(rpm) {
				b.tokens = float64(rpm)
			}
		}

		// 消耗令牌（RL-03）
		if b.tokens >= 1.0 {
			b.tokens -= 1.0
			b.mu.Unlock()
			return true, 0
		}

		// 令牌不足，计算 retryAfter（RL-03）
		missing := 1.0 - b.tokens
		refillRate := float64(b.rpm) / 60.0
		waitSec := missing / refillRate
		retry := time.Duration(math.Ceil(waitSec * float64(time.Second)))
		maxWait := time.Duration(math.Ceil(60.0 / float64(b.rpm) * float64(time.Second)))
		if retry > maxWait {
			retry = maxWait
		}
		if retry <= 0 {
			retry = time.Nanosecond
		}
		b.mu.Unlock()
		return false, retry
	}
}

// Sweep 删除 now 之前 10 分钟内未访问的桶（RL-06）。
func (l *Limiter) Sweep(now time.Time) {
	l.buckets.Range(func(key, val any) bool {
		b := val.(*bucket)
		b.mu.Lock()
		if now.Sub(b.lastSeen) > 10*time.Minute {
			b.deleted = true
			l.buckets.Delete(key)
		}
		b.mu.Unlock()
		return true
	})
}
