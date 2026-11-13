package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func expiringKey(id int64, at time.Time) *Key {
	k := key(id, 1, 10)
	k.SetExpiresAt(at)
	return k
}

// SCH-21：过期的 Key 不被选中；到期时刻本身即视为过期
func TestExpiredKeySkipped(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, expiringKey(1, t0.Add(time.Hour)), key(2, 1, 10))
	h.c.Advance(time.Hour)
	for i := 0; i < 5; i++ {
		l := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
		if l.Key.ID != 2 {
			t.Fatalf("got expired key %d", l.Key.ID)
		}
		l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	}
	st := h.s.KeyStatus()
	if !st[0].Expired || !st[0].ExpiresAt.Equal(t0.Add(time.Hour)) || st[1].Expired || !st[1].ExpiresAt.IsZero() {
		t.Fatalf("KeyStatus() = %+v", st)
	}
}

// SCH-21：未到期的 Key 正常可用
func TestNotYetExpiredKeyUsable(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, expiringKey(1, t0.Add(time.Hour)))
	h.c.Advance(time.Hour - time.Second)
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	if h.s.KeyStatus()[0].Expired {
		t.Fatal("key reported expired before its expiry")
	}
}

// SCH-21：全部过期时返回 upstream_unavailable，说明里带过期数量，不带 Retry-After
func TestAllKeysExpired(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, expiringKey(1, t0), expiringKey(2, t0.Add(-time.Hour)))
	_, err := h.s.Acquire(AcquireRequest{Pool: "p"})
	e, ok := apperr.As(err)
	if !ok || e.Code != apperr.CodeUpstreamUnavailable || e.RetryAfter != 0 || !strings.Contains(e.Message, "2 expired") {
		t.Fatalf("Acquire() error = %v", err)
	}
}

// SCH-21：固定到已过期 Key 的严格亲和请求不换 Key
func TestStrictPinOnExpiredKey(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, expiringKey(1, t0.Add(time.Hour)), key(2, 1, 10))
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p", Exclude: []int64{2}})
	l.Pin("job", 48*time.Hour)
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	h.c.Advance(2 * time.Hour)
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p", AffinityKey: "job", Strict: true}); apperr.CodeOf(err) != apperr.CodeUpstreamUnavailable {
		t.Fatalf("strict pin on expired key: error = %v, want upstream_unavailable", err)
	}
}

// SCH-21：只改过期时间时沿用旧对象（冷却、inFlight 保留），新的过期时间立即生效
func TestSetKeysExpiryIsMetadata(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, expiringKey(1, t0.Add(time.Minute)))
	inflight := mustAcquire(t, h.s, AcquireRequest{Pool: "p"})
	h.c.Advance(2 * time.Minute)
	if _, err := h.s.Acquire(AcquireRequest{Pool: "p"}); apperr.CodeOf(err) != apperr.CodeUpstreamUnavailable {
		t.Fatalf("expired key acquired: %v", err)
	}
	h.s.SetKeys([]*Key{expiringKey(1, t0.Add(24*time.Hour))}) // 管理员延长了过期时间
	st := h.s.KeyStatus()
	if st[0].Expired || st[0].InFlight != 1 {
		t.Fatalf("after extending expiry: %+v", st)
	}
	inflight.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	mustAcquire(t, h.s, AcquireRequest{Pool: "p"}).Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})

	h.s.SetKeys([]*Key{key(1, 1, 10)}) // 清除过期时间
	if st := h.s.KeyStatus(); !st[0].ExpiresAt.IsZero() {
		t.Fatalf("expiry not cleared: %+v", st)
	}
}

func TestSetExpiresAtZero(t *testing.T) {
	k := expiringKey(1, t0)
	k.SetExpiresAt(time.Time{})
	if !k.ExpiresAt().IsZero() || k.expired(t0.Add(time.Hour)) {
		t.Fatal("zero expiry must mean never expires")
	}
}

// SCH-21：LoadKeys 把库中的过期时间带到 Key 上
func TestLoadKeysCarriesExpiry(t *testing.T) {
	kr, err := crypto.NewKeyring(bytes.Repeat([]byte{7}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	sealed := func(id int64) crypto.Sealed {
		s, err := kr.Seal([]byte("sk-x"), crypto.UpstreamKeyAAD(id, "p", 1))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	exp := t0.Add(time.Hour)
	keys, err := LoadKeys(context.Background(), fakeLister{keys: []store.UpstreamKey{
		{ID: 1, Pool: "p", Name: "a", Sealed: sealed(1), Weight: 1, MaxInFlight: 1, State: store.KeyStateReady, ExpiresAt: exp},
		{ID: 2, Pool: "p", Name: "b", Sealed: sealed(2), Weight: 1, MaxInFlight: 1, State: store.KeyStateReady},
	}}, kr, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || !keys[0].ExpiresAt().Equal(exp) || !keys[1].ExpiresAt().IsZero() {
		t.Fatalf("LoadKeys() expiry = %v, %v", keys[0].ExpiresAt(), keys[1].ExpiresAt())
	}
}
