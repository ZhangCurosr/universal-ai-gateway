package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// fakeWriter 记录写入的事件；failN > 0 时前 failN 次调用返回错误。
type fakeWriter struct {
	mu     sync.Mutex
	failN  int
	calls  int
	byID   map[string]store.UsageEvent
	writes [][]store.UsageEvent
}

func newFakeWriter(failN int) *fakeWriter {
	return &fakeWriter{failN: failN, byID: map[string]store.UsageEvent{}}
}

func (w *fakeWriter) InsertUsageEvents(_ context.Context, evs []store.UsageEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls <= w.failN {
		return errors.New("disk busy")
	}
	w.writes = append(w.writes, append([]store.UsageEvent(nil), evs...))
	for _, e := range evs {
		w.byID[e.RequestID] = e // 模拟 INSERT OR IGNORE
	}
	return nil
}

func (w *fakeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.byID)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runFlusher(t *testing.T, w EventWriter, ch chan store.UsageEvent, cfg FlusherConfig) (cancel func(), done <-chan error) {
	t.Helper()
	ctx, c := context.WithCancel(context.Background())
	f := NewFlusher(w, ch, cfg, obs.Discard())
	d := make(chan error, 1)
	go func() { d <- f.Run(ctx) }()
	return c, d
}

func TestFlushByBatchSize(t *testing.T) {
	t.Skip("TODO(T06)")
	w := newFakeWriter(0)
	ch := make(chan store.UsageEvent, 100)
	cancel, done := runFlusher(t, w, ch, FlusherConfig{BatchSize: 10, Interval: time.Hour})
	defer func() { cancel(); <-done }()
	for i := 0; i < 10; i++ { // FLU-01：满 10 条立即写
		ch <- store.UsageEvent{RequestID: fmt.Sprint(i)}
	}
	waitFor(t, func() bool { return w.count() == 10 })
}

func TestFlushByInterval(t *testing.T) {
	t.Skip("TODO(T06)")
	w := newFakeWriter(0)
	ch := make(chan store.UsageEvent, 100)
	cancel, done := runFlusher(t, w, ch, FlusherConfig{BatchSize: 1000, Interval: 20 * time.Millisecond})
	defer func() { cancel(); <-done }()
	ch <- store.UsageEvent{RequestID: "only"}
	waitFor(t, func() bool { return w.count() == 1 })
}

func TestRetryKeepsBatch(t *testing.T) {
	t.Skip("TODO(T06)")
	w := newFakeWriter(2) // FLU-02：前两次失败
	ch := make(chan store.UsageEvent, 100)
	cancel, done := runFlusher(t, w, ch, FlusherConfig{BatchSize: 5, Interval: 10 * time.Millisecond, InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	defer func() { cancel(); <-done }()
	for i := 0; i < 5; i++ {
		ch <- store.UsageEvent{RequestID: fmt.Sprint(i)}
	}
	waitFor(t, func() bool { return w.count() == 5 })
}

func TestShutdownDrains(t *testing.T) {
	t.Skip("TODO(T06)")
	w := newFakeWriter(0)
	ch := make(chan store.UsageEvent, 100)
	for i := 0; i < 7; i++ {
		ch <- store.UsageEvent{RequestID: fmt.Sprint(i)}
	}
	cancel, done := runFlusher(t, w, ch, FlusherConfig{BatchSize: 1000, Interval: time.Hour})
	cancel() // FLU-04：取消后仍要把队列读空并写入
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if w.count() != 7 {
		t.Fatalf("written = %d, want 7", w.count())
	}
}

func TestShutdownWritesUnflushedFile(t *testing.T) {
	t.Skip("TODO(T06)")
	w := newFakeWriter(1 << 30) // 永远失败
	path := filepath.Join(t.TempDir(), "unflushed_usage.jsonl")
	ch := make(chan store.UsageEvent, 10)
	ch <- store.UsageEvent{RequestID: "a"}
	ch <- store.UsageEvent{RequestID: "b"}
	cancel, done := runFlusher(t, w, ch, FlusherConfig{BatchSize: 1000, Interval: time.Hour, UnflushedPath: path,
		InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, ShutdownTimeout: 50 * time.Millisecond})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unflushed file not written: %v", err)
	}
	if n := len(splitLines(data)); n != 2 {
		t.Fatalf("unflushed file has %d lines, want 2", n)
	}
}

func TestRecoverUnflushed(t *testing.T) {
	t.Skip("TODO(T06)")
	path := filepath.Join(t.TempDir(), "unflushed_usage.jsonl")
	if err := RecoverUnflushed(context.Background(), newFakeWriter(0), path); err != nil { // 文件不存在
		t.Fatalf("RecoverUnflushed(no file) = %v, want nil", err)
	}
	content := `{"RequestID":"x","ClientID":1,"CostNanoUSD":5}` + "\n" + `{"RequestID":"y","ClientID":1,"CostNanoUSD":6}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newFakeWriter(0)
	if err := RecoverUnflushed(context.Background(), w, path); err != nil { // FLU-05
		t.Fatalf("RecoverUnflushed() = %v", err)
	}
	if w.count() != 2 || w.byID["y"].CostNanoUSD != 6 {
		t.Fatalf("recovered = %+v", w.byID)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unflushed file not deleted after recovery")
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
