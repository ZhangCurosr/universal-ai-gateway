package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

type fakeRuntimeStore struct {
	mu      sync.Mutex
	saves   int
	keys    []store.KeyRuntime
	failing bool
	loadErr error
}

func (f *fakeRuntimeStore) SaveRuntimeState(_ context.Context, keys []store.KeyRuntime, _ []store.TaskPin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return errors.New("disk full")
	}
	f.saves++
	f.keys = keys
	return nil
}

func (f *fakeRuntimeStore) LoadRuntimeState(context.Context, time.Time) ([]store.KeyRuntime, []store.TaskPin, error) {
	return nil, nil, f.loadErr
}

func (f *fakeRuntimeStore) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.saves }

// RT-11、RT-12：没有变化时不写；变化后写一次；关闭时总是写最后一次；写失败下次重试
func TestRuntimeSnapshotterWritesOnlyOnChange(t *testing.T) {
	sched := scheduler.New([]scheduler.PoolConfig{{Name: "p", CooldownDefault: time.Second, CooldownMax: time.Hour}}, scheduler.Options{})
	sched.SetKeys([]*scheduler.Key{scheduler.NewKey(1, "p", "k", []byte("s"), 1, 4, scheduler.PersistedReady)})
	st := &fakeRuntimeStore{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = runtimeSnapshotter(st, sched, log, 5*time.Millisecond)(ctx) // 总是返回 nil
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	if n := st.count(); n != 1 {
		t.Fatalf("saves with no change = %d, want 1 (the first snapshot)", n)
	}
	l, err := sched.Acquire(scheduler.AcquireRequest{Pool: "p"})
	if err != nil {
		t.Fatal(err)
	}
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited, RetryAfter: time.Hour})
	time.Sleep(60 * time.Millisecond)
	if n := st.count(); n != 2 {
		t.Fatalf("saves after a change = %d, want 2", n)
	}
	st.mu.Lock()
	st.failing = true
	st.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	st.mu.Lock()
	st.failing = false
	st.mu.Unlock()
	cancel()
	<-done
	if n := st.count(); n != 3 || len(st.keys) != 1 || st.keys[0].KeyID != 1 {
		t.Fatalf("final save: count %d keys %+v", n, st.keys)
	}
}

// RT-10：读取失败不阻止启动
func TestRestoreRuntimeStateLoadError(t *testing.T) {
	sched := scheduler.New(nil, scheduler.Options{})
	restoreRuntimeState(context.Background(), &fakeRuntimeStore{loadErr: errors.New("corrupt")}, sched, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
