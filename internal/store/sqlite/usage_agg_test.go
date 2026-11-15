package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// STO-23（T79）：分组组合、按天 / 小时、+08:00 日界、过滤、Key 已不存在时 pool 为空、估算请求数、未知单位、上限。
func TestAggregateUsage(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seal := func(int64) (crypto.Sealed, error) {
		return crypto.Sealed{KEKVersion: 1, WrappedDEK: []byte("w"), Ciphertext: []byte("c")}, nil
	}
	k1, err := db.InsertUpstreamKey(ctx, sampleKey("fa"), seal) // 池 p1
	if err != nil {
		t.Fatal(err)
	}
	k2key := sampleKey("fb")
	k2key.Pool = "p2"
	k2, err := db.InsertUpstreamKey(ctx, k2key, seal)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	e := func(id string, client, key int64, model string, at time.Time, usage map[string]int64, cost int64, est bool) store.UsageEvent {
		src := store.UsageSourceUpstream
		if est {
			src = store.UsageSourceEstimated
		}
		return store.UsageEvent{RequestID: id, ClientID: client, KeyID: key, Model: model, Op: "call", Usage: usage, CostNanoUSD: cost, UsageSource: src, CreatedAt: at}
	}
	tok := func(in, out int64) map[string]int64 {
		return map[string]int64{"input_tokens": in, "output_tokens": out}
	}
	events := []store.UsageEvent{
		e("r1", 1, k1, "gpt", day.Add(1*time.Hour), tok(100, 10), 5, false),
		e("r2", 1, k1, "gpt", day.Add(2*time.Hour), tok(200, 20), 7, true),
		e("r3", 2, k2, "claude", day.Add(17*time.Hour), map[string]int64{"input_tokens": 50, "audio_input_tokens": 9}, 3, false), // 北京时间次日 01:00
		e("r4", 2, 999, "search", day.Add(25*time.Hour), map[string]int64{"credits": 2}, 1, false),                               // Key 999 不存在
		e("r5", 1, k1, "gpt", day.Add(-time.Hour), tok(1, 1), 1, false),                                                          // 区间外
	}
	if err := db.InsertUsageEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	from, to := day, day.Add(48*time.Hour)
	agg := func(q store.UsageAggQuery) []store.UsageBucket {
		t.Helper()
		if q.From.IsZero() {
			q.From, q.To = from, to
		}
		out, err := db.AggregateUsage(ctx, q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		return out
	}

	// 不分组：一行总计
	all := agg(store.UsageAggQuery{})
	if len(all) != 1 || all[0].Requests != 4 || all[0].EstimatedRequests != 1 || all[0].CostNanoUSD != 16 {
		t.Fatalf("total: %+v", all)
	}
	if u := all[0].Usage; u["input_tokens"] != 350 || u["output_tokens"] != 30 || u["audio_input_tokens"] != 9 || u["credits"] != 2 {
		t.Fatalf("total usage: %v", u)
	}

	// 按服务
	byModel := agg(store.UsageAggQuery{GroupBy: []string{store.GroupModel}})
	if len(byModel) != 3 || byModel[0].Model != "claude" || byModel[1].Model != "gpt" || byModel[1].Requests != 2 || byModel[1].Usage["input_tokens"] != 300 {
		t.Fatalf("by model: %+v", byModel)
	}

	// 按天（UTC）与按天（+08:00）
	utc := agg(store.UsageAggQuery{GroupBy: []string{store.GroupTime}, Interval: store.IntervalDay})
	if len(utc) != 2 || !utc[0].Time.Equal(day) || utc[0].Requests != 3 || utc[1].Requests != 1 {
		t.Fatalf("by day utc: %+v", utc)
	}
	cst := time.FixedZone("CST", 8*3600)
	bj := agg(store.UsageAggQuery{GroupBy: []string{store.GroupTime}, Interval: store.IntervalDay, TZ: cst})
	if len(bj) != 2 || !bj[0].Time.Equal(day.Add(-8*time.Hour)) || bj[0].Requests != 2 || bj[1].Requests != 2 {
		t.Fatalf("by day +08:00: %+v", bj)
	}

	// 按小时 + 服务；按池（Key 不存在 → ""）
	hm := agg(store.UsageAggQuery{GroupBy: []string{store.GroupTime, store.GroupModel}, Interval: store.IntervalHour})
	if len(hm) != 4 || !hm[0].Time.Equal(day.Add(time.Hour)) || hm[0].Model != "gpt" {
		t.Fatalf("by hour+model: %+v", hm)
	}
	pools := agg(store.UsageAggQuery{GroupBy: []string{store.GroupPool, store.GroupKey}})
	if len(pools) != 3 || pools[0].Pool != "" || pools[0].KeyID != 999 || pools[1].Pool != "p1" || pools[2].Pool != "p2" || pools[2].KeyID != k2 {
		t.Fatalf("by pool+key: %+v", pools)
	}

	// 过滤
	if f := agg(store.UsageAggQuery{ClientID: 2}); len(f) != 1 || f[0].Requests != 2 {
		t.Fatalf("client filter: %+v", f)
	}
	if f := agg(store.UsageAggQuery{Pool: "p1", GroupBy: []string{store.GroupClient}}); len(f) != 1 || f[0].ClientID != 1 || f[0].Requests != 2 {
		t.Fatalf("pool filter: %+v", f)
	}
	if f := agg(store.UsageAggQuery{Model: "nothing"}); len(f) != 0 || f == nil {
		t.Fatalf("empty result: %+v", f)
	}

	// 参数错误
	bad := []store.UsageAggQuery{
		{From: to, To: from},
		{From: from, To: from.Add(93 * 24 * time.Hour)},
		{From: from, To: to, GroupBy: []string{"protocol"}},
		{From: from, To: to, GroupBy: []string{store.GroupModel, store.GroupModel}},
		{From: from, To: to, GroupBy: []string{store.GroupTime}},
		{From: from, To: from.Add(8 * 24 * time.Hour), GroupBy: []string{store.GroupTime}, Interval: store.IntervalHour},
		{From: from, To: to, GroupBy: []string{store.GroupTime, store.GroupModel, store.GroupKey, store.GroupClient}, Interval: store.IntervalDay},
	}
	for _, q := range bad {
		if _, err := db.AggregateUsage(ctx, q); !errors.Is(err, store.ErrInvalidArgument) {
			t.Errorf("%+v: %v", q, err)
		}
	}
}

// 结果行数上限：超过 MaxUsageAggBuckets 行 → ErrTooManyBuckets。
func TestAggregateUsageTooManyBuckets(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	var events []store.UsageEvent
	for i := 0; i <= store.MaxUsageAggBuckets; i++ {
		events = append(events, store.UsageEvent{RequestID: "r" + strings.Repeat("x", i%3) + time.Duration(i).String(), ClientID: int64(i + 1), KeyID: 1, Model: "m",
			Op: "call", Usage: map[string]int64{"requests": 1}, UsageSource: store.UsageSourceUpstream, CreatedAt: base})
	}
	if err := db.InsertUsageEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	_, err := db.AggregateUsage(ctx, store.UsageAggQuery{From: base, To: base.Add(time.Hour), GroupBy: []string{store.GroupClient}})
	if !errors.Is(err, store.ErrTooManyBuckets) {
		t.Fatalf("err = %v", err)
	}
}
