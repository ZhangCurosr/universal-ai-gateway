package scheduler

import (
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

func testBreaker() *breaker {
	b := &breaker{}
	b.configure(config.BreakerConfig{Window: 10 * time.Second, MinRequests: 4, FailureRatio: 0.5,
		OpenDuration: 30 * time.Second, MaxOpenDuration: 100 * time.Second})
	return b
}

// 窗口过期后旧的失败不再计入
func TestBreakerWindowExpires(t *testing.T) {
	b := testBreaker()
	for i := 0; i < 3; i++ {
		b.record(false, false, t0)
	}
	later := t0.Add(11 * time.Second)
	for i := 0; i < 3; i++ {
		b.record(false, false, later)
	}
	if st := b.current(later); st != BreakerClosed {
		t.Fatalf("state = %s, want closed (old failures expired, only 3 in window)", st)
	}
	b.record(false, false, later)
	if st := b.current(later); st != BreakerOpen {
		t.Fatalf("state = %s, want open after 4 failures in window", st)
	}
}

// MinRequests 未达到时不打开；失败率不足时不打开
func TestBreakerMinRequestsAndRatio(t *testing.T) {
	b := testBreaker()
	for i := 0; i < 3; i++ {
		b.record(false, false, t0)
	}
	if !b.allow(t0) {
		t.Fatal("opened before MinRequests")
	}
	b2 := testBreaker()
	for i := 0; i < 10; i++ {
		b2.record(i < 4, false, t0) // 4 成功 6 失败 → 60% ≥ 50%
	}
	if b2.allow(t0) {
		t.Fatal("did not open at 60% failures")
	}
	b3 := testBreaker()
	for i := 0; i < 10; i++ {
		b3.record(i < 6, false, t0) // 40% 失败
	}
	if !b3.allow(t0) {
		t.Fatal("opened at 40% failures")
	}
}

// 探测失败后打开时长翻倍，且不超过上限；探测成功后关闭并重置
func TestBreakerProbeBackoff(t *testing.T) {
	b := testBreaker()
	for i := 0; i < 4; i++ {
		b.record(false, false, t0)
	}
	now := t0
	for _, wantOpen := range []time.Duration{60 * time.Second, 100 * time.Second, 100 * time.Second} {
		now = now.Add(b.openFor)
		probe, ok := b.onAcquire(now)
		if !probe || !ok {
			t.Fatalf("at %v: onAcquire = %v, %v; want probe", now, probe, ok)
		}
		if _, ok := b.onAcquire(now); ok {
			t.Fatal("second concurrent probe allowed")
		}
		b.record(false, true, now)
		if b.openFor != wantOpen || b.current(now) != BreakerOpen {
			t.Fatalf("after failed probe: openFor = %v state = %s, want %v open", b.openFor, b.current(now), wantOpen)
		}
	}
	now = now.Add(b.openFor)
	if probe, _ := b.onAcquire(now); !probe {
		t.Fatal("no probe after open period")
	}
	b.record(true, true, now)
	if b.current(now) != BreakerClosed || b.openFor != 30*time.Second {
		t.Fatalf("after successful probe: state = %s openFor = %v", b.current(now), b.openFor)
	}
}

// 探测请求以不计入统计的结果结束时释放名额
func TestBreakerCancelProbe(t *testing.T) {
	b := testBreaker()
	for i := 0; i < 4; i++ {
		b.record(false, false, t0)
	}
	now := t0.Add(31 * time.Second)
	if probe, _ := b.onAcquire(now); !probe {
		t.Fatal("expected probe")
	}
	b.cancelProbe()
	if probe, ok := b.onAcquire(now); !probe || !ok {
		t.Fatal("probe slot not released by cancelProbe")
	}
}
