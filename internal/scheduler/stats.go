package scheduler

import (
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// 每把 Key 最近 24 小时的尝试统计（spec/07 §7.5，KS-01 ~ KS-03，任务卡 T65）。只在内存中，重启后从零开始。

// statFailKinds 是计为 Key 失败的结果（KS-02）；下标即计数数组的下标。
var statFailKinds = []protocol.OutcomeKind{
	protocol.OutcomeRateLimited, protocol.OutcomeAuthFailed, protocol.OutcomePermissionDenied,
	protocol.OutcomeOverloaded, protocol.OutcomeServerError, protocol.OutcomeTimeout, protocol.OutcomeNetworkError,
}

const statHours = 24

type statBucket struct {
	hour     atomic.Int64 // Unix 小时数；与当前小时不同时视为空桶
	attempts atomic.Int64
	fails    [7]atomic.Int64 // 与 statFailKinds 一一对应
}

// keyStats 由 Key 持有指针，SetKeys 以新对象替换同 ID 的 Key 时沿用（KS-03）。
type keyStats struct {
	buckets      [statHours]statBucket
	lastFailKind atomic.Int32 // protocol.OutcomeKind + 1；0 = 没有失败
	lastFailAt   atomic.Int64 // UnixNano
}

func failIndex(k protocol.OutcomeKind) int {
	for i, f := range statFailKinds {
		if f == k {
			return i
		}
	}
	return -1
}

// record 实现 KS-01、KS-02：只做原子操作。轮换桶时先用比较并交换占住新的小时，再清零计数；
// 两步之间并发写入的极少数计数可能丢失（统计用途，可以接受）。
func (s *keyStats) record(now time.Time, kind protocol.OutcomeKind) {
	fi := failIndex(kind)
	if kind != protocol.OutcomeSuccess && fi < 0 {
		return // client_error、canceled、local_error 等不是 Key 的问题
	}
	h := now.Unix() / 3600
	b := &s.buckets[h%statHours]
	for {
		cur := b.hour.Load()
		if cur == h {
			break
		}
		if cur > h { // 时钟回拨：丢弃这一次
			return
		}
		if b.hour.CompareAndSwap(cur, h) {
			b.attempts.Store(0)
			for i := range b.fails {
				b.fails[i].Store(0)
			}
			break
		}
	}
	b.attempts.Add(1)
	if fi >= 0 {
		b.fails[fi].Add(1)
		s.lastFailKind.Store(int32(kind) + 1)
		s.lastFailAt.Store(now.UnixNano())
	}
}

// KeyStat 是一把 Key 最近 24 小时（含当前小时）的尝试统计。
type KeyStat struct {
	Attempts     int64
	Failures     map[string]int64 // 结果名（protocol.OutcomeKind.String）→ 次数；只含 > 0 的
	LastFailure  string           // 最近一次失败的种类；没有时为空
	LastFailedAt time.Time
}

// FailureRate 返回失败次数 ÷ 尝试次数；没有尝试时为 0。
func (st KeyStat) FailureRate() float64 {
	if st.Attempts == 0 {
		return 0
	}
	var n int64
	for _, v := range st.Failures {
		n += v
	}
	return float64(n) / float64(st.Attempts)
}

func (s *keyStats) snapshot(now time.Time) KeyStat {
	h := now.Unix() / 3600
	out := KeyStat{Failures: map[string]int64{}}
	for i := range s.buckets {
		b := &s.buckets[i]
		if bh := b.hour.Load(); bh > h-statHours && bh <= h {
			out.Attempts += b.attempts.Load()
			for j := range b.fails {
				if n := b.fails[j].Load(); n > 0 {
					out.Failures[statFailKinds[j].String()] += n
				}
			}
		}
	}
	if k := s.lastFailKind.Load(); k > 0 {
		out.LastFailure = protocol.OutcomeKind(k - 1).String()
		out.LastFailedAt = time.Unix(0, s.lastFailAt.Load())
	}
	return out
}

// KeyStats 返回全部 Key 最近 24 小时的尝试统计（KS-03）。只读内存。
func (s *Scheduler) KeyStats() map[int64]KeyStat {
	now := s.opts.Now()
	s.mu.RLock()
	keys := make([]*Key, 0, len(s.byID))
	for _, k := range s.byID {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	out := make(map[int64]KeyStat, len(keys))
	for _, k := range keys {
		out[k.ID] = k.stats.snapshot(now)
	}
	return out
}
