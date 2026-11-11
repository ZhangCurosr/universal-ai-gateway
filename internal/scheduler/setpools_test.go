package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func namedPool(name, strategy string) PoolConfig {
	p := testPool(strategy)
	p.Name = name
	return p
}

// SCH-20：新池加入后，其 Key 经 SetKeys 开始被调度
func TestSetPoolsAddsPool(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	q := NewKey(2, "q", "kq", []byte("sq"), 1, 10, PersistedReady)
	h.s.SetKeys([]*Key{key(1, 1, 10), q}) // q 的池尚不存在：被忽略
	if _, err := h.s.Acquire(AcquireRequest{Pool: "q"}); err == nil {
		t.Fatal("unknown pool acquired")
	}
	h.s.SetPools([]PoolConfig{testPool(config.StrategyLeastInflight), namedPool("q", config.StrategyLeastInflight)})
	h.s.SetKeys([]*Key{key(1, 1, 10), q})
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "q"})
	if l.Key.ID != 2 {
		t.Fatalf("acquired key %d", l.Key.ID)
	}
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
}

// SCH-20：已有池更新参数时 Key 的运行时状态（冷却）保留，新参数生效
func TestSetPoolsKeepsKeyState(t *testing.T) {
	k1 := key(1, 1, 10)
	h := newHarness(t, config.StrategyLeastInflight, k1)
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited, RetryAfter: 20 * time.Second})

	changed := testPool(config.StrategyWeightedRoundRobin)
	changed.CooldownDefault = 5 * time.Second
	h.s.SetPools([]PoolConfig{changed})
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); err == nil {
		t.Fatal("cooldown lost after SetPools")
	}
	h.c.Advance(21 * time.Second)
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	if l.Key != k1 {
		t.Fatal("key object replaced")
	}
	// 新的 cooldown.default（5s）生效：RateLimited 且无 RetryAfter
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited})
	h.c.Advance(6 * time.Second)
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
}

// SCH-20：删除池后停止调度、Key 从状态中移除；同名池恢复后重新加载即回归
func TestSetPoolsRemovesPool(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10))
	held := mustAcquire(t, h.s, AcquireRequest{Pool: "p"}) // 删除前发出的租约
	h.s.SetPools(nil)
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); err == nil {
		t.Fatal("removed pool still schedulable")
	}
	if st := h.s.KeyStatus(); len(st) != 0 {
		t.Fatalf("KeyStatus = %+v", st)
	}
	held.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess}) // 不 panic

	h.s.SetPools([]PoolConfig{testPool(config.StrategyLeastInflight)})
	h.s.SetKeys([]*Key{key(1, 1, 10)})
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
}

// SCH-20：与 Acquire、Finish、SetKeys 并发（race 检测在 Linux 上验证）
func TestSetPoolsConcurrent(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 100), key(2, 1, 100))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if l, err := h.s.Acquire(AcquireRequest{Pool: "p"}); err == nil {
					l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		strategy := config.StrategyLeastInflight
		if i%2 == 0 {
			strategy = config.StrategyWeightedRoundRobin
		}
		h.s.SetPools([]PoolConfig{testPool(strategy)})
		h.s.SetKeys([]*Key{key(1, 1, 100), key(2, 1, 100)})
		_ = h.s.KeyStatus() // 只为制造并发读，结果不重要
	}
	close(stop)
	wg.Wait()
	for _, st := range h.s.KeyStatus() {
		if st.InFlight != 0 {
			t.Errorf("key %d in-flight = %d after all leases finished", st.ID, st.InFlight)
		}
	}
}
