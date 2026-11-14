package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func sched(t *testing.T, period string, r *config.QuotaReset) config.QuotaSchedule {
	t.Helper()
	s, err := (&config.QuotaConfig{Period: period, Reset: r}).Schedule()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// QT-02：各周期的开始时刻与下一个重置时刻
func TestQuotaPeriods(t *testing.T) {
	cst := "+08:00"
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name        string
		period      string
		reset       *config.QuotaReset
		now         string
		start, next string
	}{
		{"day before reset", "day", &config.QuotaReset{Time: "08:00", TZ: cst}, "2026-11-13T07:59:00+08:00", "2026-11-12T08:00:00+08:00", "2026-11-13T08:00:00+08:00"},
		{"day at reset", "day", &config.QuotaReset{Time: "08:00", TZ: cst}, "2026-11-13T08:00:00+08:00", "2026-11-13T08:00:00+08:00", "2026-11-14T08:00:00+08:00"},
		{"day default utc", "day", nil, "2026-11-13T23:00:00+08:00", "2026-11-13T00:00:00Z", "2026-11-14T00:00:00Z"},
		{"week same day before", "week", &config.QuotaReset{Weekday: "fri", Time: "12:00"}, "2026-11-13T11:00:00Z", "2026-11-06T12:00:00Z", "2026-11-13T12:00:00Z"},
		{"week later", "week", &config.QuotaReset{Weekday: "mon"}, "2026-11-13T11:00:00Z", "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z"},
		{"month", "month", &config.QuotaReset{Day: 1, TZ: cst}, "2026-11-13T00:00:00Z", "2026-11-01T00:00:00+08:00", "2026-12-01T00:00:00+08:00"},
		{"month before reset day", "month", &config.QuotaReset{Day: 15}, "2026-11-13T00:00:00Z", "2026-10-15T00:00:00Z", "2026-11-15T00:00:00Z"},
		{"month end clamp feb", "month", &config.QuotaReset{Day: 31}, "2027-02-28T12:00:00Z", "2027-02-28T00:00:00Z", "2027-03-31T00:00:00Z"},
		{"month end clamp before", "month", &config.QuotaReset{Day: 31}, "2027-02-27T12:00:00Z", "2027-01-31T00:00:00Z", "2027-02-28T00:00:00Z"},
		{"month across year", "month", &config.QuotaReset{Day: 1}, "2027-01-01T00:00:00Z", "2027-01-01T00:00:00Z", "2027-02-01T00:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &Quota{Schedule: sched(t, tt.period, tt.reset)}
			now := at(tt.now)
			if got := q.PeriodStart(now); !got.Equal(at(tt.start)) {
				t.Errorf("PeriodStart = %v, want %s", got, tt.start)
			}
			if got := q.NextReset(now); !got.Equal(at(tt.next)) {
				t.Errorf("NextReset = %v, want %s", got, tt.next)
			}
		})
	}
	q := &Quota{Schedule: sched(t, "total", nil)}
	if !q.PeriodStart(at("2030-01-01T00:00:00Z")).Equal(totalPeriodStart) || !q.NextReset(t0).IsZero() {
		t.Error("total period must never reset")
	}
}

func quotaHarness(t *testing.T, drain bool, limit int64, keys ...*Key) *harness {
	t.Helper()
	pc := testPool(config.StrategyWeightedRoundRobin)
	pc.Quota = &Quota{Unit: "credits", Limit: limit, Drain: drain, Schedule: sched(t, "day", nil)}
	c := &clock{t: t0.Add(time.Hour)}
	s := New([]PoolConfig{pc}, Options{Now: c.Now})
	s.SetKeys(keys)
	return &harness{s: s, c: c}
}

// use 选一把 Key，成功结束并记 n 个额度单位，返回 Key ID。
func use(t *testing.T, h *harness, n int64) int64 {
	t.Helper()
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	h.s.RecordUsage(l.Key.ID, map[string]int64{"credits": n, "other": 100})
	return l.Key.ID
}

// QT-03 ~ QT-05、QT-08：drain 用完一把再换下一把；全部用完后返回带重置时刻的 503；跨过重置时刻后恢复
func TestQuotaDrain(t *testing.T) {
	h := quotaHarness(t, true, 3, key(1, 1, 10), key(2, 1, 10), key(3, 1, 10))
	var got []int64
	for i := 0; i < 9; i++ {
		got = append(got, use(t, h, 1))
	}
	// 剩余相同时按 ID：第一次选 1，之后 1 剩余最少，一直用到用满
	want := []int64{1, 1, 1, 2, 2, 2, 3, 3, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("drain order = %v, want %v", got, want)
		}
	}
	_, err := h.s.Acquire(AcquireRequest{Pool: "p"})
	e, ok := apperr.As(err)
	if !ok || e.Code != apperr.CodeUpstreamUnavailable || !strings.Contains(e.Message, "3 quota exhausted") || e.RetryAfter != 23*time.Hour {
		t.Fatalf("exhausted error = %v (retry after %v)", err, e.RetryAfter)
	}
	st := h.s.KeyStatus()
	if st[0].QuotaUsed != 3 || st[0].QuotaLimit != 3 || !st[0].QuotaExhausted || !st[0].QuotaResetAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("KeyStatus = %+v", st[0])
	}
	h.c.Advance(23 * time.Hour) // 到达重置时刻
	if id := use(t, h, 1); id != 1 {
		t.Fatalf("after reset got key %d, want 1", id)
	}
	if st := h.s.KeyStatus(); st[1].QuotaUsed != 0 || st[0].QuotaUsed != 1 || st[0].QuotaExhausted {
		t.Fatalf("after reset = %+v %+v", st[0], st[1])
	}
}

// QT-05：spread 总是选剩余最多的
func TestQuotaSpread(t *testing.T) {
	h := quotaHarness(t, false, 10, key(1, 1, 10), key(2, 1, 10), key(3, 1, 10))
	var got []int64
	for i := 0; i < 6; i++ {
		got = append(got, use(t, h, 2))
	}
	want := []int64{1, 2, 3, 1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spread order = %v, want %v", got, want)
		}
	}
}

// QT-04：软上限——用量超过上限后也只是不再选；满载的 Key 让给下一把
func TestQuotaSoftLimitAndFullKey(t *testing.T) {
	h := quotaHarness(t, true, 5, key(1, 1, 1), key(2, 1, 1))
	if id := use(t, h, 7); id != 1 { // 一次就超出
		t.Fatalf("first key = %d", id)
	}
	held := mustAcquire(t, h.s, AcquireRequest{Pool: "p"}) // 只剩 2；占满它
	if held.Key.ID != 2 {
		t.Fatalf("got %d, want 2", held.Key.ID)
	}
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); apperr.CodeOf(err) != apperr.CodeUpstreamUnavailable {
		t.Fatalf("want 503 while 1 is exhausted and 2 is full, got %v", err)
	}
	held.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	if st := h.s.KeyStatus(); st[0].QuotaUsed != 7 || !st[0].QuotaExhausted {
		t.Fatalf("status = %+v", st[0])
	}
}

// QT-04：严格亲和（任务查询）不检查额度；普通会话亲和在用满后换 Key
func TestQuotaStrictAffinityIgnoresQuota(t *testing.T) {
	h := quotaHarness(t, true, 1, key(1, 1, 10), key(2, 1, 10))
	submit := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "sess"})
	submit.Pin("job", time.Hour)
	submit.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	h.s.RecordUsage(submit.Key.ID, map[string]int64{"credits": 1})
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "job", Strict: true})
	if l.Key.ID != submit.Key.ID {
		t.Fatalf("strict poll went to %d, want %d", l.Key.ID, submit.Key.ID)
	}
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	l = mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "sess"})
	if l.Key.ID == submit.Key.ID {
		t.Fatal("session affinity must move off an exhausted key")
	}
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
}

// QT-06：重算结果覆盖计数；周期不符时忽略。QT-03：未知 Key、无额度池忽略。SCH-16：换新对象时保留用量
func TestSetQuotaUsage(t *testing.T) {
	h := quotaHarness(t, true, 10, key(1, 1, 10), key(2, 1, 10))
	qp := h.s.QuotaPools()["p"]
	start := qp.PeriodStart(h.c.Now())
	if !h.s.SetQuotaUsage("p", start, map[int64]int64{1: 9}) {
		t.Fatal("SetQuotaUsage refused the current period")
	}
	if h.s.SetQuotaUsage("p", start.Add(-24*time.Hour), map[int64]int64{2: 5}) || h.s.SetQuotaUsage("nope", start, nil) {
		t.Fatal("SetQuotaUsage must refuse another period or an unknown pool")
	}
	h.s.RecordUsage(99, map[string]int64{"credits": 1})
	h.s.RecordUsage(1, map[string]int64{"credits": -3})
	if st := h.s.KeyStatus(); st[0].QuotaUsed != 9 || st[1].QuotaUsed != 0 {
		t.Fatalf("after set = %+v %+v", st[0], st[1])
	}
	// 权重变化：SetKeys 换成新对象，额度用量随之转移
	h.s.SetKeys([]*Key{key(1, 5, 10), key(2, 1, 10)})
	if st := h.s.KeyStatus(); st[0].QuotaUsed != 9 {
		t.Fatalf("usage lost on SetKeys: %+v", st[0])
	}
	if got := h.s.PoolKeyIDs("p"); len(got) != 2 || got[0] != 1 || h.s.PoolKeyIDs("nope") != nil {
		t.Fatalf("PoolKeyIDs = %v", got)
	}

	// 没有额度的池：RecordUsage 无效，KeyStatus 不带额度
	plain := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	plain.s.RecordUsage(1, map[string]int64{"credits": 5})
	if st := plain.s.KeyStatus(); st[0].QuotaLimit != 0 || st[0].QuotaUsed != 0 || len(plain.s.QuotaPools()) != 0 {
		t.Fatalf("plain pool status = %+v", st[0])
	}
}

// quotaFromConfig：nil 与非法配置都视为不限
func TestQuotaFromConfig(t *testing.T) {
	if quotaFromConfig(nil) != nil || quotaFromConfig(&config.QuotaConfig{Period: "year"}) != nil {
		t.Fatal("nil or invalid config must disable the quota")
	}
	q := quotaFromConfig(&config.QuotaConfig{Unit: "pages", Limit: 3, Period: "total", Strategy: config.QuotaSpread})
	if q == nil || q.Drain || q.Limit != 3 {
		t.Fatalf("quota = %+v", q)
	}
}
