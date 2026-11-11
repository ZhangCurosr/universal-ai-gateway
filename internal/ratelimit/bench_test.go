package ratelimit

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// BenchmarkAllow 测试令牌桶限流器在单个 key 与 1000 个 key 下的吞吐（spec/02 §10.1 BEN-01，spec/09 §3）。
func BenchmarkAllow(b *testing.B) {
	const rpm = 100_000_000 // 足够大的限额，使基准持续放行

	b.Run("SingleKey", func(b *testing.B) {
		b.ReportAllocs()
		lim := New(nil)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				lim.Allow("c:1", rpm)
			}
		})
	})

	b.Run("1000Keys", func(b *testing.B) {
		b.ReportAllocs()
		lim := New(nil)
		keys := make([]string, 1000)
		for i := range keys {
			keys[i] = fmt.Sprintf("c:%d", i)
			lim.Allow(keys[i], rpm) // 预先初始化 bucket，消除初次分配抖动
		}

		var counter atomic.Uint64
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				idx := counter.Add(1) % 1000
				lim.Allow(keys[idx], rpm)
			}
		})
	})
}
