package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func TestUnlimited(t *testing.T) {
	l := New(newClock().Now)
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("c:1", 0); !ok { // RL-01
			t.Fatalf("Allow(rpm=0) #%d = false", i)
		}
	}
}

func TestBurstThenReject(t *testing.T) {
	l := New(newClock().Now)
	for i := 0; i < 60; i++ { // RL-02：初始满桶
		if ok, _ := l.Allow("c:1", 60); !ok {
			t.Fatalf("Allow #%d rejected within burst", i)
		}
	}
	ok, retry := l.Allow("c:1", 60) // RL-03
	if ok {
		t.Fatal("61st Allow = true, want false")
	}
	if retry <= 0 || retry > time.Second {
		t.Fatalf("retryAfter = %v, want (0, 1s] for 1 token/s refill", retry)
	}
}

func TestRefill(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	for i := 0; i < 60; i++ {
		l.Allow("c:1", 60)
	}
	c.Advance(2 * time.Second) // 补充 2 个
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("c:1", 60); !ok {
			t.Fatalf("Allow after refill #%d = false", i)
		}
	}
	if ok, _ := l.Allow("c:1", 60); ok {
		t.Fatal("third Allow after 2s = true, want false")
	}
}

func TestRPMChange(t *testing.T) {
	l := New(newClock().Now)
	l.Allow("c:1", 100) // 桶容量 100，剩 99
	allowed := 0
	for i := 0; i < 50; i++ { // RL-04：容量降为 10，令牌截断到 10
		if ok, _ := l.Allow("c:1", 10); ok {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed after rpm change = %d, want 10", allowed)
	}
}

func TestSweep(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	l.Allow("c:1", 60)
	c.Advance(11 * time.Minute)
	l.Sweep(c.Now()) // RL-06
	n := 0
	l.buckets.Range(func(any, any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("buckets after sweep = %d, want 0", n)
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := New(newClock().Now) // 时钟不动，只有初始的 600 个令牌
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if ok, _ := l.Allow("c:1", 600); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 600 {
		t.Fatalf("allowed = %d, want exactly 600", got)
	}
}

func TestNewDefaultClock(t *testing.T) {
	l := New(nil)
	if l == nil {
		t.Fatal("New(nil) = nil")
	}
	ok, _ := l.Allow("c:default", 60)
	if !ok {
		t.Fatal("Allow with default clock = false, want true")
	}
}

func TestNegativeRPM(t *testing.T) {
	l := New(newClock().Now)
	if ok, retry := l.Allow("c:neg", -10); !ok || retry != 0 {
		t.Fatalf("Allow(rpm=-10) = (%v, %v), want (true, 0)", ok, retry)
	}
}

func TestRPMChangeIncrease(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	for i := 0; i < 10; i++ {
		l.Allow("c:1", 10)
	}
	// All 10 consumed, tokens = 0.
	// Now change RPM to 60. Current tokens should be min(0, 60) = 0.
	ok, _ := l.Allow("c:1", 60)
	if ok {
		t.Fatal("Allow immediately after increasing RPM should be rejected when tokens=0")
	}
	// Advance 1s -> refills 60/60 = 1 token.
	c.Advance(time.Second)
	ok, _ = l.Allow("c:1", 60)
	if !ok {
		t.Fatal("Allow after 1s refill with new RPM=60 should be allowed")
	}
}

func TestRefillCappedAtCapacity(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	l.Allow("c:1", 10) // remaining 9
	// Advance by 1 hour -> refill would be 600 tokens if uncapped, but should cap at 10.
	c.Advance(time.Hour)
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("c:1", 10); !ok {
			t.Fatalf("Allow #%d failed within capped burst", i)
		}
	}
	if ok, _ := l.Allow("c:1", 10); ok {
		t.Fatal("11th Allow succeeded, want false (refill should cap at capacity)")
	}
}

func TestSweepRetentionAndRecreate(t *testing.T) {
	c := newClock()
	l := New(c.Now)
	l.Allow("c:old", 60)
	c.Advance(5 * time.Minute)
	l.Allow("c:new", 60)
	c.Advance(6 * time.Minute) // c:old is 11m old, c:new is 6m old
	l.Sweep(c.Now())

	if _, ok := l.buckets.Load("c:old"); ok {
		t.Fatal("c:old should have been swept")
	}
	if _, ok := l.buckets.Load("c:new"); !ok {
		t.Fatal("c:new should have been retained")
	}

	// Recreate bucket for c:old
	if ok, _ := l.Allow("c:old", 60); !ok {
		t.Fatal("Allow after sweep should create new bucket and succeed")
	}
}
