package ledger

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// BenchmarkReserveSettle 测试内存账本的预留与结算吞吐（spec/02 §10.1 BEN-01，spec/06 §3）。
func BenchmarkReserveSettle(b *testing.B) {
	b.ReportAllocs()

	m := NewMemory(MemoryConfig{
		EventQueueCapacity:     131072,
		AdmissionHighWatermark: 0.95,
	})
	m.LoadAccounts([]Account{
		{
			ClientID:     1,
			Unlimited:    true,
			QuotaNanoUSD: 1_000_000_000_000,
		},
	})

	stopDrain := make(chan struct{})
	var drained atomic.Int64
	var drainWg sync.WaitGroup
	drainWg.Add(1)

	go func() {
		defer drainWg.Done()
		for {
			select {
			case <-stopDrain:
				for {
					select {
					case <-m.Events():
						drained.Add(1)
					default:
						return
					}
				}
			case <-m.Events():
				drained.Add(1)
			}
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reqID := fmt.Sprintf("req-%d", i)
		r, err := m.Reserve(1, reqID, 1000)
		if err != nil {
			b.Fatalf("Reserve failed at %d: %v", i, err)
		}
		ev := store.UsageEvent{
			RequestID:   reqID,
			ClientID:    1,
			CostNanoUSD: 1000,
		}
		if err := m.Settle(r, ev); err != nil {
			b.Fatalf("Settle failed at %d: %v", i, err)
		}
	}

	b.StopTimer()
	close(stopDrain)
	drainWg.Wait()

	if drained.Load() != int64(b.N) {
		b.Fatalf("events lost: drained=%d, want=%d", drained.Load(), b.N)
	}
}
