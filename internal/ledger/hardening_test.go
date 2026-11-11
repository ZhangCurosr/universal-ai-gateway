package ledger

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// LED-05：Reserve 与 Settle/Release/Upsert 交错，实际费用均不超过预留。
func TestMixedAccountUpdatesNeverOverAdmit(t *testing.T) {
	const quota, workers, rounds = 2000, 64, 100
	m := NewMemory(MemoryConfig{EventQueueCapacity: workers * rounds})
	m.LoadAccounts([]Account{{ClientID: 1, QuotaNanoUSD: quota}})
	var charged atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range rounds {
				id := fmt.Sprintf("%d-%d", worker, i)
				r, err := m.Reserve(1, id, 1)
				if err != nil {
					if apperr.CodeOf(err) != apperr.CodeInsufficientQuota {
						t.Errorf("Reserve() error = %v, want insufficient_quota", err)
					}
					continue
				}
				runtime.Gosched()
				if i%4 == 0 {
					m.Release(r)
				} else {
					if err := m.Settle(r, costEvent(id, 1)); err != nil {
						t.Errorf("Settle() error = %v", err)
					}
					charged.Add(1)
				}
				if i%7 == 0 {
					m.UpsertAccount(Account{ClientID: 1, QuotaNanoUSD: quota})
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	s, _ := m.Snapshot(1)
	if s.SpentNanoUSD != quota || charged.Load() != quota || s.ReservedNanoUSD != 0 {
		t.Fatalf("mixed updates: snapshot=%+v charged=%d, want spent=%d reserved=0", s, charged.Load(), quota)
	}
	if len(m.Events()) != quota {
		t.Fatalf("billing events = %d, want %d", len(m.Events()), quota)
	}
}

func TestAccountArithmeticDoesNotWrap(t *testing.T) {
	m := NewMemory(MemoryConfig{EventQueueCapacity: 10})
	m.LoadAccounts([]Account{{ClientID: 1, Unlimited: true, SpentNanoUSD: math.MaxInt64 - 1}})
	r, err := m.Reserve(1, "large", math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(1, "overflow", 1); apperr.CodeOf(err) != apperr.CodeOverloaded {
		t.Fatalf("Reserve overflowing total = %v, want overloaded", err)
	}
	if err := m.Settle(r, costEvent("large", 2)); err != nil {
		t.Fatal(err)
	}
	m.UpsertAccount(Account{ClientID: 1, QuotaNanoUSD: math.MaxInt64})
	if _, err := m.Reserve(1, "exhausted", 1); apperr.CodeOf(err) != apperr.CodeInsufficientQuota {
		t.Fatalf("Reserve after saturated spending = %v, want insufficient_quota", err)
	}
	s, _ := m.Snapshot(1)
	if s.SpentNanoUSD != math.MaxInt64 || s.ReservedNanoUSD != 0 {
		t.Fatalf("snapshot after saturation = %+v", s)
	}
}

// LED-10：Snapshot 不得把两个不同版本的 quota/unlimited 拼成一个不存在的账户状态。
func TestAccountSnapshotIsOneVersion(t *testing.T) {
	m := NewMemory(MemoryConfig{})
	m.LoadAccounts([]Account{{ClientID: 1, Unlimited: true, QuotaNanoUSD: 1}})
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 10000 {
			m.UpsertAccount(Account{ClientID: 1, Unlimited: false, QuotaNanoUSD: 2})
			m.UpsertAccount(Account{ClientID: 1, Unlimited: true, QuotaNanoUSD: 1})
		}
	}()
	close(start)
	var invalid *AccountSnapshot
	for range 30000 {
		s, _ := m.Snapshot(1)
		if (s.Unlimited && s.QuotaNanoUSD != 1) || (!s.Unlimited && s.QuotaNanoUSD != 2) {
			invalid = &s
			break
		}
	}
	wg.Wait()
	if invalid != nil {
		t.Fatalf("Snapshot returned mixed account versions: %+v", *invalid)
	}
}
