package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func quotaSched(t *testing.T) *scheduler.Scheduler {
	t.Helper()
	cfg := []config.PoolConfig{
		{Name: "q", Quota: &config.QuotaConfig{Unit: "credits", Limit: 5, Period: config.QuotaPeriodTotal, Strategy: config.QuotaDrain}},
		{Name: "plain"},
	}
	s := scheduler.New(scheduler.PoolsFromConfig(cfg), scheduler.Options{})
	s.SetKeys([]*scheduler.Key{
		scheduler.NewKey(1, "q", "a", []byte("s1"), 1, 4, scheduler.PersistedReady),
		scheduler.NewKey(2, "q", "b", []byte("s2"), 1, 4, scheduler.PersistedReady),
		scheduler.NewKey(3, "plain", "c", []byte("s3"), 1, 4, scheduler.PersistedReady),
	})
	return s
}

func usedOf(s *scheduler.Scheduler) map[int64]int64 {
	out := map[int64]int64{}
	for _, st := range s.KeyStatus() {
		out[st.ID] = st.QuotaUsed
	}
	return out
}

type failingLedger struct{ ledger.Ledger }

func (failingLedger) Settle(*ledger.Reservation, store.UsageEvent) error {
	return errors.New("queue full")
}

// QT-12：结算后计入额度；结算出错时仍计入并返回原错误
func TestQuotaLedger(t *testing.T) {
	s := quotaSched(t)
	mem := ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: 8})
	mem.UpsertAccount(ledger.Account{ClientID: 7, Unlimited: true})
	ql := quotaLedger{Ledger: mem, sched: s}
	r, err := ql.Reserve(7, "req-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ql.Settle(r, store.UsageEvent{RequestID: "req-1", ClientID: 7, KeyID: 1, Usage: map[string]int64{"credits": 2}}); err != nil {
		t.Fatal(err)
	}
	if err := (quotaLedger{Ledger: failingLedger{}, sched: s}).Settle(nil, store.UsageEvent{KeyID: 1, Usage: map[string]int64{"credits": 1}}); err == nil {
		t.Fatal("the underlying error must be returned")
	}
	if u := usedOf(s); u[1] != 3 || u[2] != 0 {
		t.Fatalf("used = %v", u)
	}
	if len(mem.Events()) == 0 {
		t.Fatal("the wrapped ledger must still produce the billing event")
	}
}

type fakeUsageStore struct {
	used  map[int64]int64
	err   error
	calls []string
}

func (f *fakeUsageStore) SumKeyUsage(_ context.Context, unit string, since time.Time, ids []int64) (map[int64]int64, error) {
	f.calls = append(f.calls, unit)
	if !since.Equal(time.Unix(0, 0)) || len(ids) != 2 {
		return nil, errors.New("unexpected arguments")
	}
	return f.used, f.err
}

// QT-10：启动时重算覆盖计数并记日志；失败只记 Warn，计数保持不变；没有额度的池不查询
func TestRecomputeQuotaUsage(t *testing.T) {
	s := quotaSched(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	st := &fakeUsageStore{used: map[int64]int64{1: 5}}
	recomputeQuotaUsage(context.Background(), st, s, nil, log)
	if u := usedOf(s); u[1] != 5 || u[2] != 0 || len(st.calls) != 1 || st.calls[0] != "credits" {
		t.Fatalf("used = %v calls %v", u, st.calls)
	}
	if !strings.Contains(logs.String(), `"msg":"quota usage restored"`) || !strings.Contains(logs.String(), `"exhausted":1`) {
		t.Fatalf("log = %s", logs.String())
	}
	logs.Reset()
	recomputeQuotaUsage(context.Background(), &fakeUsageStore{err: errors.New("disk")}, s, []string{"q", "plain", "gone"}, log)
	if u := usedOf(s); u[1] != 5 || !strings.Contains(logs.String(), "quota usage recompute failed") {
		t.Fatalf("after failure used = %v log %s", u, logs.String())
	}
}

// QT-11：新增额度或单位、周期、重置时刻变化时重算；只改上限或策略时不重算
func TestQuotaPoolsToRecompute(t *testing.T) {
	q := func(unit string, limit int64, period, tz string) *config.QuotaConfig {
		c := &config.QuotaConfig{Unit: unit, Limit: limit, Period: period, Strategy: config.QuotaDrain}
		if tz != "" {
			c.Reset = &config.QuotaReset{TZ: tz}
		}
		return c
	}
	mk := func(qs ...*config.QuotaConfig) *config.Config {
		c := &config.Config{}
		for i, x := range qs {
			c.Pools = append(c.Pools, config.PoolConfig{Name: string(rune('a' + i)), Quota: x})
		}
		return c
	}
	old := mk(q("credits", 10, "day", "+08:00"), q("credits", 10, "day", ""), q("pages", 1, "day", ""), nil, q("credits", 1, "total", ""))
	neu := mk(q("credits", 99, "day", "+08:00"), q("credits", 10, "day", "UTC"), q("credits", 1, "day", ""), q("credits", 1, "day", ""), q("credits", 1, "total", ""))
	neu.Pools[0].Quota.Strategy = config.QuotaSpread
	got := quotaPoolsToRecompute(old, neu)
	if strings.Join(got, ",") != "b,c,d" {
		t.Fatalf("recompute = %v, want b,c,d", got)
	}
	if got := quotaPoolsToRecompute(nil, mk(q("credits", 1, "day", ""))); len(got) != 1 {
		t.Fatalf("first generation = %v", got)
	}
}
