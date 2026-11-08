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
	t.Skip("TODO(T04)")
	l := New(newClock().Now)
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow(1, 0); !ok { // RL-01
			t.Fatalf("Allow(rpm=0) #%d = false", i)
		}
	}
}

func TestBurstThenReject(t *testing.T) {
	t.Skip("TODO(T04)")
	l := New(newClock().Now)
	for i := 0; i < 60; i++ { // RL-02：初始满桶
		if ok, _ := l.Allow(1, 60); !ok {
			t.Fatalf("Allow #%d rejected within burst", i)
		}
	}
	ok, retry := l.Allow(1, 60) // RL-03
	if ok {
		t.Fatal("61st Allow = true, want false")
	}
	if retry <= 0 || retry > time.Second {
		t.Fatalf("retryAfter = %v, want (0, 1s] for 1 token/s refill", retry)
	}
}

func TestRefill(t *testing.T) {
	t.Skip("TODO(T04)")
	c := newClock()
	l := New(c.Now)
	for i := 0; i < 60; i++ {
		l.Allow(1, 60)
	}
	c.Advance(2 * time.Second) // 补充 2 个
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow(1, 60); !ok {
			t.Fatalf("Allow after refill #%d = false", i)
		}
	}
	if ok, _ := l.Allow(1, 60); ok {
		t.Fatal("third Allow after 2s = true, want false")
	}
}

func TestRPMChange(t *testing.T) {
	t.Skip("TODO(T04)")
	l := New(newClock().Now)
	l.Allow(1, 100) // 桶容量 100，剩 99
	allowed := 0
	for i := 0; i < 50; i++ { // RL-04：容量降为 10，令牌截断到 10
		if ok, _ := l.Allow(1, 10); ok {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed after rpm change = %d, want 10", allowed)
	}
}

func TestSweep(t *testing.T) {
	t.Skip("TODO(T04)")
	c := newClock()
	l := New(c.Now)
	l.Allow(1, 60)
	c.Advance(11 * time.Minute)
	l.Sweep(c.Now()) // RL-06
	n := 0
	l.buckets.Range(func(any, any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("buckets after sweep = %d, want 0", n)
	}
}

func TestConcurrentAllow(t *testing.T) {
	t.Skip("TODO(T04)")
	l := New(newClock().Now) // 时钟不动，只有初始的 600 个令牌
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if ok, _ := l.Allow(1, 600); ok {
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
