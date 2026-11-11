package scheduler

import (
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// BenchmarkAcquire 测试调度器在 8 个 Key 池中的租约获取与释放吞吐（spec/02 §10.1 BEN-01，spec/07 §2）。
func BenchmarkAcquire(b *testing.B) {
	makeKeys := func() []*Key {
		keys := make([]*Key, 8)
		for i := 0; i < 8; i++ {
			// 8 个 Key，不同权重与极大的最大并发数，确保不触发容量饱和
			keys[i] = NewKey(int64(i+1), "p", "k", []byte("secret"), i+1, 1_000_000, PersistedReady)
		}
		return keys
	}

	b.Run("least_inflight", func(b *testing.B) {
		b.ReportAllocs()
		pCfg := PoolConfig{
			Name:            "p",
			Strategy:        config.StrategyLeastInflight,
			AffinityTTL:     5 * time.Minute,
			CooldownDefault: 30 * time.Second,
			CooldownMax:     10 * time.Minute,
			Breaker: config.BreakerConfig{
				Window:          60 * time.Second,
				MinRequests:     10,
				FailureRatio:    0.5,
				OpenDuration:    30 * time.Second,
				MaxOpenDuration: 5 * time.Minute,
			},
		}
		s := New([]PoolConfig{pCfg}, Options{})
		s.SetKeys(makeKeys())

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			req := AcquireRequest{Pool: "p"}
			for pb.Next() {
				l, err := s.Acquire(req)
				if err != nil {
					b.Fatalf("Acquire failed: %v", err)
				}
				l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
			}
		})
	})

	b.Run("weighted_round_robin", func(b *testing.B) {
		b.ReportAllocs()
		pCfg := PoolConfig{
			Name:            "p",
			Strategy:        config.StrategyWeightedRoundRobin,
			AffinityTTL:     5 * time.Minute,
			CooldownDefault: 30 * time.Second,
			CooldownMax:     10 * time.Minute,
			Breaker: config.BreakerConfig{
				Window:          60 * time.Second,
				MinRequests:     10,
				FailureRatio:    0.5,
				OpenDuration:    30 * time.Second,
				MaxOpenDuration: 5 * time.Minute,
			},
		}
		s := New([]PoolConfig{pCfg}, Options{})
		s.SetKeys(makeKeys())

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			req := AcquireRequest{Pool: "p"}
			for pb.Next() {
				l, err := s.Acquire(req)
				if err != nil {
					b.Fatalf("Acquire failed: %v", err)
				}
				l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
			}
		})
	})
}
