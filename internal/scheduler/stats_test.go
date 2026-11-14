package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// KS-01、KS-02：各种结果的计数；不是 Key 的问题的结果不计入
func TestKeyStatsCounts(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 100))
	finish := func(kind protocol.OutcomeKind) {
		mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: kind})
		h.s.SetKeys([]*Key{key(1, 1, 100)}) // 解除冷却与熔断的影响：同身份的 Key 沿用对象
		k := h.s.byID[1]
		k.cooldownUntil.Store(0)
		k.authFailures.Store(0)
		k.persisted.Store(int32(PersistedReady))
	}
	for _, kind := range []protocol.OutcomeKind{protocol.OutcomeSuccess, protocol.OutcomeSuccess, protocol.OutcomeRateLimited,
		protocol.OutcomeTimeout, protocol.OutcomeClientError, protocol.OutcomeCanceled, protocol.OutcomeLocalError} {
		finish(kind)
	}
	h.c.Advance(time.Minute)
	finish(protocol.OutcomeAuthFailed)
	st := h.s.KeyStats()[1]
	if st.Attempts != 5 || st.Failures["rate_limited"] != 1 || st.Failures["timeout"] != 1 || st.Failures["auth_failed"] != 1 || len(st.Failures) != 3 {
		t.Fatalf("stats = %+v", st)
	}
	if st.LastFailure != "auth_failed" || !st.LastFailedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("last failure = %q %v", st.LastFailure, st.LastFailedAt)
	}
	if r := st.FailureRate(); r != 0.6 {
		t.Fatalf("failure rate = %v", r)
	}
	if (KeyStat{}).FailureRate() != 0 {
		t.Fatal("no attempts must give rate 0")
	}
}

// KS-01：满 24 小时的旧数据不再计入；同一个桶跨天复用时先清零
func TestKeyStatsWindow(t *testing.T) {
	s := &keyStats{}
	s.record(t0, protocol.OutcomeSuccess)
	s.record(t0.Add(30*time.Minute), protocol.OutcomeServerError)
	s.record(t0.Add(23*time.Hour), protocol.OutcomeSuccess)
	if st := s.snapshot(t0.Add(23 * time.Hour)); st.Attempts != 3 {
		t.Fatalf("within 24h = %+v", st)
	}
	if st := s.snapshot(t0.Add(24 * time.Hour)); st.Attempts != 1 || len(st.Failures) != 0 {
		t.Fatalf("after 24h = %+v", st)
	}
	s.record(t0.Add(24*time.Hour), protocol.OutcomeSuccess) // 复用第 0 小时的桶
	if st := s.snapshot(t0.Add(24 * time.Hour)); st.Attempts != 2 {
		t.Fatalf("reused bucket = %+v", st)
	}
	s.record(t0, protocol.OutcomeSuccess) // 时钟回拨到已轮换的小时：丢弃
	if st := s.snapshot(t0.Add(24 * time.Hour)); st.Attempts != 2 {
		t.Fatalf("stale record counted: %+v", st)
	}
}

// KS-01：同一小时内并发记录不丢；KS-03：SetKeys 换新对象时沿用统计
func TestKeyStatsConcurrentAndCarried(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 1000))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l, err := h.s.Acquire(AcquireRequest{Pool: "p"})
				if err != nil {
					t.Error(err)
					return
				}
				l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
			}
		}()
	}
	wg.Wait()
	if st := h.s.KeyStats()[1]; st.Attempts != 800 {
		t.Fatalf("attempts = %d, want 800", st.Attempts)
	}
	h.s.SetKeys([]*Key{key(1, 7, 1000)}) // 权重变化：新对象
	if st := h.s.KeyStats()[1]; st.Attempts != 800 {
		t.Fatalf("stats lost on SetKeys: %d", st.Attempts)
	}
}
