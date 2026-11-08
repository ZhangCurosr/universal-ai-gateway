package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testPool(strategy string) PoolConfig {
	return PoolConfig{
		Name: "p", Strategy: strategy, AffinityTTL: 5 * time.Minute,
		CooldownDefault: 30 * time.Second, CooldownMax: 10 * time.Minute,
		Breaker: config.BreakerConfig{Window: 60 * time.Second, MinRequests: 4, FailureRatio: 0.5,
			OpenDuration: 30 * time.Second, MaxOpenDuration: 5 * time.Minute},
	}
}

type harness struct {
	s           *Scheduler
	c           *clock
	quarantined chan int64
}

func newHarness(t *testing.T, strategy string, keys ...*Key) *harness {
	t.Helper()
	c := &clock{t: t0}
	q := make(chan int64, 10)
	s := New([]PoolConfig{testPool(strategy)}, Options{Now: c.Now, OnQuarantine: func(id int64, _ string) { q <- id }})
	s.SetKeys(keys)
	return &harness{s: s, c: c, quarantined: q}
}

func key(id int64, weight, maxInFlight int) *Key {
	return NewKey(id, "p", "k", []byte("secret"), weight, maxInFlight, PersistedReady)
}

func mustAcquire(t *testing.T, s *Scheduler, req AcquireRequest) *Lease {
	t.Helper()
	l, err := s.Acquire(req)
	if err != nil {
		t.Fatalf("Acquire(%+v) error = %v", req, err)
	}
	return l
}

// SCH-03 / ST-05
func TestRateLimitedCooldown(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited, RetryAfter: 20 * time.Second})
	_, err := h.s.Acquire(AcquireRequest{Pool: "p"})
	e, ok := apperr.As(err)
	if !ok || e.Code != apperr.CodeUpstreamUnavailable || e.RetryAfter != 20*time.Second { // §3.1
		t.Fatalf("Acquire during cooldown error = %v, want upstream_unavailable with RetryAfter 20s", err)
	}
	h.c.Advance(21 * time.Second)
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
}

// SCH-03：冷却中的 Key 被跳过
func TestAcquireSkipsCooldown(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10), key(2, 1, 10))
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	cooled := l.Key.ID
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited})
	for i := 0; i < 5; i++ {
		l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
		if l.Key.ID == cooled {
			t.Fatalf("selected key %d during cooldown", cooled)
		}
		l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	}
}

// SCH-05
func TestAcquireRespectsMaxInFlight(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 2))
	a := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); apperr.CodeOf(err) != apperr.CodeUpstreamUnavailable {
		t.Fatalf("third Acquire error = %v, want upstream_unavailable", err)
	}
	a.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
}

// SCH-10
func TestAffinityPreferred(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10), key(2, 1, 10))
	first := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "sess"})
	first.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	busy := mustAcquire(t, h.s, AcquireRequest{Pool: "p"}) // 让绑定的 Key 负载更高
	defer busy.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	for i := 0; i < 10; i++ {
		l := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "sess"})
		if l.Key.ID != first.Key.ID {
			t.Fatalf("affinity broken: got key %d, want %d", l.Key.ID, first.Key.ID)
		}
		l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	}
}

// SCH-10：绑定的 Key 满载时回落到其他 Key
func TestAffinityFallbackWhenBusy(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 1), key(2, 1, 1))
	a := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "sess"})
	b := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "sess"})
	if a.Key.ID == b.Key.ID {
		t.Fatal("second request reused a full key")
	}
}

// SCH-01：Exclude
func TestExcludeTried(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10), key(2, 1, 10))
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p", Exclude: []int64{1}})
	if l.Key.ID != 2 {
		t.Fatalf("got key %d, want 2", l.Key.ID)
	}
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p", Exclude: []int64{1, 2}}); err == nil {
		t.Fatal("Acquire with all keys excluded: error = nil")
	}
}

// SCH-11
func TestLeastInflightOrder(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10), key(2, 1, 10))
	a := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	b := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	if a.Key.ID == b.Key.ID {
		t.Fatal("least_inflight picked the busier key")
	}
}

// SCH-12
func TestWeightedRoundRobinDistribution(t *testing.T) {
	h := newHarness(t, config.StrategyWeightedRoundRobin, key(1, 5, 100), key(2, 1, 100))
	counts := map[int64]int{}
	for i := 0; i < 6000; i++ {
		l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
		counts[l.Key.ID]++
		l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	}
	if counts[1] < 4950 || counts[1] > 5050 {
		t.Fatalf("distribution = %v, want ~5000:1000", counts)
	}
}

// LED 同理：Finish 只生效一次
func TestFinishOnce(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	if l.Key.InFlight() != 0 {
		t.Fatalf("inFlight = %d after double Finish, want 0", l.Key.InFlight())
	}
}

// ST-06
func TestAuthFailedTwiceQuarantines(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeAuthFailed})
	h.c.Advance(61 * time.Second) // 第一次只冷却 60s
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeAuthFailed})
	select {
	case id := <-h.quarantined:
		if id != 1 {
			t.Fatalf("quarantined id = %d", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnQuarantine not called")
	}
	h.c.Advance(time.Hour)
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); err == nil {
		t.Fatal("quarantined key still selectable")
	}
}

// ST-08
func TestOverloadedNoPenalty(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	for i := 0; i < 20; i++ {
		mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeOverloaded})
	}
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
}

// §3.1：没有任何冷却中的 Key 时 RetryAfter = 0
func TestNoKeyRetryAfter(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight)
	_, err := h.s.Acquire(AcquireRequest{Pool: "p"})
	e, ok := apperr.As(err)
	if !ok || e.Code != apperr.CodeUpstreamUnavailable || e.RetryAfter != 0 {
		t.Fatalf("Acquire(empty pool) error = %v", err)
	}
}

// §5.1：熔断打开 → 半开放行一个探测 → 成功后关闭
func TestBreakerOpensAndHalfOpen(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	for i := 0; i < 4; i++ { // MinRequests=4，全部失败
		mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeServerError})
	}
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); err == nil {
		t.Fatal("breaker did not open")
	}
	h.c.Advance(31 * time.Second)
	probe := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); err == nil { // 半开只放 1 个
		t.Fatal("half-open allowed a second concurrent request")
	}
	probe.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	a := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	b := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	a.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	b.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
}

// SCH-16：导出字段不变 → 复用旧对象（保留 inFlight）
func TestSetKeysPreservesRuntime(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	h.s.SetKeys([]*Key{key(1, 1, 10)}) // 例如另一个 Key 被新增后整体重载
	st := h.s.KeyStatus()
	if len(st) != 1 || st[0].InFlight != 1 {
		t.Fatalf("KeyStatus() = %+v, want inFlight preserved", st)
	}
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	if st := h.s.KeyStatus(); st[0].InFlight != 0 {
		t.Fatalf("inFlight after Finish = %d", st[0].InFlight)
	}
}

// SCH-16：导出字段改变 → 新对象，冷却保留，inFlight 从 0 开始
func TestSetKeysChangedFieldsKeepCooldown(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited, RetryAfter: time.Minute})
	h.s.SetKeys([]*Key{key(1, 3, 10)}) // 管理员改了权重
	st := h.s.KeyStatus()
	if len(st) != 1 || st[0].CooldownRemaining <= 0 {
		t.Fatalf("KeyStatus() = %+v, want cooldown preserved", st)
	}
}

// SCH-17 / SCH-18：任务固定后只能用创建它的 Key
func TestStrictPin(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10), key(2, 1, 10))
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p", AffinityKey: "job", Strict: true}); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("strict miss error = %v, want not_found", err)
	}
	submit := mustAcquire(t, h.s, AcquireRequest{Pool: "p", Exclude: []int64{1}}) // 落在 Key 2
	submit.Pin("job", time.Hour)
	submit.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	for i := 0; i < 5; i++ {
		l := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "job", Strict: true})
		if l.Key.ID != 2 {
			t.Fatalf("strict pin got key %d, want 2", l.Key.ID)
		}
		l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	}
	// 被固定的 Key 不可用时不得换 Key
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p", AffinityKey: "job", Strict: true, Exclude: []int64{2}}); apperr.CodeOf(err) != apperr.CodeUpstreamUnavailable {
		t.Fatalf("strict pin with key excluded: error = %v, want upstream_unavailable", err)
	}
	if pool, ok := h.s.PinnedPool("job"); !ok || pool != "p" { // SCH-19
		t.Fatalf("PinnedPool(job) = %q, %v", pool, ok)
	}
	h.c.Advance(2 * time.Hour) // 过期
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p", AffinityKey: "job", Strict: true}); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("expired pin error = %v, want not_found", err)
	}
	if _, ok := h.s.PinnedPool("job"); ok {
		t.Fatal("PinnedPool(expired) ok = true")
	}
}
