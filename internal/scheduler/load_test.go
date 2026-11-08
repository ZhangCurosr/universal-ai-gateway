package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

type fakeLister struct {
	keys []store.UpstreamKey
	err  error
}

func (f fakeLister) ListUpstreamKeys(context.Context) ([]store.UpstreamKey, error) {
	return f.keys, f.err
}

func TestLoadKeysSkipsUndecryptable(t *testing.T) {
	kr, err := crypto.NewKeyring(bytes.Repeat([]byte{7}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	good, err := kr.Seal([]byte("sk-good"), crypto.UpstreamKeyAAD(1, "p", 1))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := kr.Seal([]byte("sk-bad"), crypto.UpstreamKeyAAD(99, "p", 1)) // AAD 绑定的是另一个 ID
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	keys, err := LoadKeys(context.Background(), fakeLister{keys: []store.UpstreamKey{
		{ID: 1, Pool: "p", Name: "a", Sealed: good, Weight: 2, MaxInFlight: 5, State: store.KeyStateReady},
		{ID: 2, Pool: "p", Name: "b", Sealed: bad, Weight: 1, MaxInFlight: 1, State: store.KeyStateReady},
	}}, kr, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ID != 1 || string(keys[0].Secret) != "sk-good" || keys[0].Weight != 2 || keys[0].MaxInFlight != 5 {
		t.Fatalf("LoadKeys() = %+v", keys)
	}
	if !strings.Contains(logBuf.String(), "key_id=2") || strings.Contains(logBuf.String(), "sk-") {
		t.Fatalf("log = %q (must name key 2 and must not contain secrets)", logBuf.String())
	}
	if _, err := LoadKeys(context.Background(), fakeLister{err: errors.New("db down")}, kr, log); err == nil {
		t.Fatal("store error not returned")
	}
}

// SCH-14：并发 Acquire 不超过 MaxInFlight
func TestAcquireConcurrentNeverExceeds(t *testing.T) {
	s := New([]PoolConfig{testPool(config.StrategyLeastInflight)}, Options{})
	s.SetKeys([]*Key{key(1, 1, 3), key(2, 1, 2)})
	var peak, cur atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := s.Acquire(AcquireRequest{Pool: "p"})
			if err != nil {
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			cur.Add(-1)
			l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
		}()
	}
	wg.Wait()
	if peak.Load() > 5 {
		t.Fatalf("peak concurrent leases = %d, want <= 5", peak.Load())
	}
	for _, st := range s.KeyStatus() {
		if st.InFlight != 0 {
			t.Fatalf("key %d inFlight = %d after all leases finished", st.ID, st.InFlight)
		}
	}
}

// SetKeys 忽略未知池的 Key；PinnedPool 在 Key 被移除后返回 false
func TestSetKeysUnknownPoolAndPinnedPoolRemovedKey(t *testing.T) {
	s := New([]PoolConfig{testPool(config.StrategyLeastInflight)}, Options{})
	s.SetKeys([]*Key{key(1, 1, 3), NewKey(5, "nope", "x", []byte("s"), 1, 1, PersistedReady)})
	if st := s.KeyStatus(); len(st) != 1 {
		t.Fatalf("KeyStatus() = %+v, want only key 1", st)
	}
	l, err := s.Acquire(AcquireRequest{Pool: "p"})
	if err != nil {
		t.Fatal(err)
	}
	l.Pin("job", time.Hour)
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	s.SetKeys(nil)
	if _, ok := s.PinnedPool("job"); ok {
		t.Fatal("PinnedPool() ok for a removed key")
	}
	if _, err := s.Acquire(AcquireRequest{Pool: "unknown"}); err == nil {
		t.Fatal("Acquire(unknown pool) succeeded")
	}
}
