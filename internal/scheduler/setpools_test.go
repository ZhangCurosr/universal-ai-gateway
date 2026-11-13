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

// SCH-12 / SCH-20：已读取旧池快照的请求与新一代共用同一份轮询进度。
func TestSetPoolsWeightedRoundRobinAcrossGenerations(t *testing.T) {
	h := newHarness(t, config.StrategyWeightedRoundRobin, key(1, 5, 10), key(2, 1, 10))
	generations := []*pool{(*h.s.pools.Load())["p"]}
	for i := 0; i < 3; i++ {
		cfg := testPool(config.StrategyWeightedRoundRobin)
		cfg.CooldownDefault += time.Duration(i+1) * time.Second
		h.s.SetPools([]PoolConfig{cfg})
		generations = append(generations, (*h.s.pools.Load())["p"])
	}

	start := make(chan struct{})
	results := make(chan [2]int, len(generations))
	var wg sync.WaitGroup
	for _, p := range generations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var counts [2]int
			for i := 0; i < 600; i++ {
				// 模拟 Acquire 已取到该代池与候选集，热重载后继续执行选择。
				l := h.s.pickSWRR(p, p.keys, t0)
				if l == nil {
					t.Error("pickSWRR() = nil, want available lease")
					return
				}
				counts[l.Key.ID-1]++
				l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
			}
			results <- counts // 容量等于 worker 数，不阻塞退出。
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var total [2]int
	for counts := range results {
		total[0] += counts[0]
		total[1] += counts[1]
	}
	if total != [2]int{2000, 400} {
		t.Fatalf("weighted selections across generations = %v, want [2000 400]", total)
	}
	for _, st := range h.s.KeyStatus() {
		if st.InFlight != 0 {
			t.Errorf("key %d in-flight = %d after all leases finished, want 0", st.ID, st.InFlight)
		}
	}
}

// SCH-20：共享轮询状态不能改变旧租约持有的冷却参数。
func TestSetPoolsKeepsLeaseConfig(t *testing.T) {
	h := newHarness(t, config.StrategyWeightedRoundRobin, key(1, 1, 10), key(2, 1, 10))
	old := mustAcquire(t, h.s, AcquireRequest{Pool: "p", Exclude: []int64{2}})
	cfg := testPool(config.StrategyWeightedRoundRobin)
	cfg.CooldownDefault = 5 * time.Second
	h.s.SetPools([]PoolConfig{cfg})
	current := mustAcquire(t, h.s, AcquireRequest{Pool: "p", Exclude: []int64{1}})
	old.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited})
	current.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited})
	for _, tt := range []struct {
		lease *Lease
		want  time.Duration
	}{{old, 30 * time.Second}, {current, 5 * time.Second}} {
		if got := tt.lease.Key.CooldownUntil().Sub(t0); got != tt.want {
			t.Errorf("key %d cooldown after Finish() = %v, want %v", tt.lease.Key.ID, got, tt.want)
		}
	}
}
