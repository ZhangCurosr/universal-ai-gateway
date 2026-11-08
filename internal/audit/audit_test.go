package audit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

type fakeStore struct {
	mu      sync.Mutex
	recs    []store.AuditRecord
	batches int
	before  []time.Time
}

func (f *fakeStore) InsertAuditRecords(_ context.Context, recs []store.AuditRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	f.recs = append(f.recs, recs...)
	return nil
}

func (f *fakeStore) DeleteAuditBefore(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before = append(f.before, before)
	return 0, nil
}

func (f *fakeStore) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}

func TestSubmitDropsWhenFull(t *testing.T) {
	t.Skip("TODO(T13)")
	p := NewPipeline(2)
	for i := 0; i < 5; i++ {
		p.Submit(store.AuditRecord{RequestID: fmt.Sprint(i)}) // AUD-01：不得阻塞
	}
	if p.Dropped() != 3 {
		t.Fatalf("Dropped() = %d, want 3", p.Dropped())
	}
}

func TestWriterBatches(t *testing.T) {
	t.Skip("TODO(T13)")
	p := NewPipeline(100)
	st := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewWriter(p, st, WriterConfig{BatchSize: 3, FlushInterval: 20 * time.Millisecond}, obs.Discard()).Run(ctx)
	}()
	for i := 0; i < 7; i++ {
		p.Submit(store.AuditRecord{RequestID: fmt.Sprint(i)})
	}
	deadline := time.Now().Add(5 * time.Second)
	for st.n() < 7 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if st.n() != 7 {
		t.Fatalf("written = %d, want 7", st.n())
	}
}

func TestWriterDrainsOnShutdown(t *testing.T) {
	t.Skip("TODO(T13)")
	p := NewPipeline(100)
	for i := 0; i < 4; i++ {
		p.Submit(store.AuditRecord{RequestID: fmt.Sprint(i)})
	}
	st := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // AUD-03
	if err := NewWriter(p, st, WriterConfig{BatchSize: 100, FlushInterval: time.Hour}, obs.Discard()).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st.n() != 4 {
		t.Fatalf("written = %d, want 4", st.n())
	}
}

func TestRetentionDeletesOld(t *testing.T) {
	t.Skip("TODO(T13)")
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	st := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	r := &Retention{St: st, RetentionDays: 30, Interval: time.Hour, Now: func() time.Time { return now }, Log: obs.Discard()}
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.before)
		st.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(st.before) == 0 || !st.before[0].Equal(now.AddDate(0, 0, -30)) { // AUD-04：启动时立即执行
		t.Fatalf("DeleteAuditBefore calls = %v, want first = now-30d", st.before)
	}
}
