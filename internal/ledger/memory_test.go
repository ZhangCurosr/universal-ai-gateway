package ledger

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func newTestMemory(capacity int) *Memory {
	m := NewMemory(MemoryConfig{EventQueueCapacity: capacity, AdmissionHighWatermark: 0.9})
	m.LoadAccounts([]Account{
		{ClientID: 1, QuotaNanoUSD: 1000},
		{ClientID: 2, Unlimited: true},
	})
	return m
}

func costEvent(reqID string, cost int64) store.UsageEvent {
	return store.UsageEvent{RequestID: reqID, ClientID: 1, CostNanoUSD: cost, UsageSource: store.UsageSourceUpstream}
}

func wantCode(t *testing.T, err error, code apperr.Code) {
	t.Helper()
	if got := apperr.CodeOf(err); got != code {
		t.Fatalf("error code = %q (%v), want %q", got, err, code)
	}
}

func TestReserveWithinQuota(t *testing.T) {
	m := newTestMemory(16)
	r, err := m.Reserve(1, "r1", 400)
	if err != nil || r.Amount != 400 {
		t.Fatalf("Reserve() = %+v, %v", r, err)
	}
	s, _ := m.Snapshot(1)
	if s.ReservedNanoUSD != 400 || s.SpentNanoUSD != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestReserveExceedsQuota(t *testing.T) {
	m := newTestMemory(16)
	if _, err := m.Reserve(1, "r1", 800); err != nil {
		t.Fatal(err)
	}
	_, err := m.Reserve(1, "r2", 300) // LED-03：800 + 300 > 1000
	wantCode(t, err, apperr.CodeInsufficientQuota)
	if s, _ := m.Snapshot(1); s.ReservedNanoUSD != 800 {
		t.Fatalf("failed Reserve modified state: %+v", s)
	}
}

func TestReserveUnknownClient(t *testing.T) {
	_, err := newTestMemory(16).Reserve(99, "r", 1) // LED-02
	wantCode(t, err, apperr.CodeUnauthenticated)
}

func TestUnlimited(t *testing.T) {
	m := newTestMemory(16)
	if _, err := m.Reserve(2, "r", 1<<50); err != nil {
		t.Fatalf("unlimited Reserve() error = %v", err)
	}
}

func TestSettleOverdraft(t *testing.T) {
	m := newTestMemory(16)
	r, _ := m.Reserve(1, "r1", 100)
	if err := m.Settle(r, costEvent("r1", 1500)); err != nil { // LED-06：允许透支
		t.Fatalf("Settle() error = %v", err)
	}
	s, _ := m.Snapshot(1)
	if s.SpentNanoUSD != 1500 || s.ReservedNanoUSD != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	if got := <-m.Events(); got.RequestID != "r1" || got.CostNanoUSD != 1500 {
		t.Fatalf("event = %+v", got)
	}
	_, err := m.Reserve(1, "r2", 1)
	wantCode(t, err, apperr.CodeInsufficientQuota)
}

func TestSettleTwice(t *testing.T) {
	m := newTestMemory(16)
	r, _ := m.Reserve(1, "r1", 100)
	if err := m.Settle(r, costEvent("r1", 10)); err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(r, costEvent("r1", 10)); !errors.Is(err, ErrAlreadyFinished) { // LED-08
		t.Fatalf("second Settle() error = %v, want ErrAlreadyFinished", err)
	}
	m.Release(r) // 不应生效
	if s, _ := m.Snapshot(1); s.SpentNanoUSD != 10 || s.ReservedNanoUSD != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	if len(m.Events()) != 1 {
		t.Fatalf("events = %d, want 1", len(m.Events()))
	}
}

func TestSettleNegativeCost(t *testing.T) {
	m := newTestMemory(16)
	r, _ := m.Reserve(1, "r1", 100)
	wantCode(t, m.Settle(r, costEvent("r1", -1)), apperr.CodeInternal) // LED-11
}

func TestReleaseRestores(t *testing.T) {
	m := newTestMemory(16)
	r, _ := m.Reserve(1, "r1", 1000)
	m.Release(r) // LED-09
	m.Release(r)
	if s, _ := m.Snapshot(1); s.ReservedNanoUSD != 0 || s.SpentNanoUSD != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	if len(m.Events()) != 0 {
		t.Fatal("Release produced an event")
	}
	if _, err := m.Reserve(1, "r2", 1000); err != nil {
		t.Fatalf("Reserve after Release error = %v", err)
	}
}

func TestAdmissionBackpressure(t *testing.T) {
	m := newTestMemory(10) // 高水位 = 9
	for i := 0; i < 9; i++ {
		r, err := m.Reserve(2, fmt.Sprint(i), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Settle(r, store.UsageEvent{RequestID: fmt.Sprint(i), ClientID: 2}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := m.Reserve(2, "x", 1) // LED-04
	wantCode(t, err, apperr.CodeOverloaded)
}

func TestConcurrentReserveNeverExceeds(t *testing.T) {
	m := NewMemory(MemoryConfig{EventQueueCapacity: 16})
	m.LoadAccounts([]Account{{ClientID: 1, QuotaNanoUSD: 500}})
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Reserve(1, "r", 1); err == nil { // LED-05
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 500 {
		t.Fatalf("successful reserves = %d, want exactly 500", ok.Load())
	}
}

func TestUpsertKeepsSpent(t *testing.T) {
	m := newTestMemory(16)
	r, _ := m.Reserve(1, "r1", 100)
	_ = m.Settle(r, costEvent("r1", 300))
	<-m.Events()
	r2, _ := m.Reserve(1, "r2", 50)
	m.UpsertAccount(Account{ClientID: 1, QuotaNanoUSD: 5000}) // LED-10
	s, _ := m.Snapshot(1)
	if s.QuotaNanoUSD != 5000 || s.SpentNanoUSD != 300 || s.ReservedNanoUSD != 50 {
		t.Fatalf("snapshot after upsert = %+v", s)
	}
	m.Release(r2)
	m.UpsertAccount(Account{ClientID: 3, QuotaNanoUSD: 10, SpentNanoUSD: 4})
	if s, ok := m.Snapshot(3); !ok || s.SpentNanoUSD != 4 {
		t.Fatalf("new account snapshot = %+v, %v", s, ok)
	}
}

// LED-01：amount <= 0 按 1 预留
func TestReserveNonPositiveAmount(t *testing.T) {
	m := newTestMemory(16)
	r, err := m.Reserve(1, "r", 0)
	if err != nil || r.Amount != 1 {
		t.Fatalf("Reserve(0) = %+v, %v", r, err)
	}
}

// LoadAccounts 覆盖已有账户；结算时账户已不存在仍发送事件
func TestLoadAccountsReplacesAndSettleWithoutAccount(t *testing.T) {
	m := newTestMemory(16)
	r, _ := m.Reserve(1, "r1", 10)
	m.LoadAccounts([]Account{{ClientID: 5, QuotaNanoUSD: 1}})
	if _, ok := m.Snapshot(1); ok {
		t.Fatal("old account survived LoadAccounts")
	}
	if err := m.Settle(r, costEvent("r1", 7)); err != nil {
		t.Fatal(err)
	}
	if ev := <-m.Events(); ev.CostNanoUSD != 7 {
		t.Fatalf("event = %+v", ev)
	}
}

// LED-11：负成本不消耗 Reservation，之后仍可正常结算
func TestSettleNegativeCostLeavesReservationOpen(t *testing.T) {
	m := newTestMemory(16)
	r, _ := m.Reserve(1, "r1", 100)
	_ = m.Settle(r, costEvent("r1", -1))
	if err := m.Settle(r, costEvent("r1", 5)); err != nil {
		t.Fatalf("Settle after rejected negative cost: %v", err)
	}
}

func TestNewMemoryDefaults(t *testing.T) {
	m := NewMemory(MemoryConfig{})
	if cap(m.events) != 4096 || m.cfg.AdmissionHighWatermark != 0.9 {
		t.Fatalf("defaults = %d, %v", cap(m.events), m.cfg.AdmissionHighWatermark)
	}
}
