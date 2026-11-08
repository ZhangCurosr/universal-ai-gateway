package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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
	p := NewPipeline(2)
	for i := 0; i < 5; i++ {
		p.Submit(store.AuditRecord{RequestID: fmt.Sprint(i)}) // AUD-01：不得阻塞
	}
	if p.Dropped() != 3 {
		t.Fatalf("Dropped() = %d, want 3", p.Dropped())
	}
}

func TestWriterBatches(t *testing.T) {
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

type failStore struct {
	mu       sync.Mutex
	recs     []store.AuditRecord
	fail     bool
	attempts int
	err      error
}

func (s *failStore) InsertAuditRecords(_ context.Context, recs []store.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.fail {
		return s.err
	}
	s.recs = append(s.recs, recs...)
	return nil
}

func (s *failStore) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func (s *failStore) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

func (s *failStore) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func TestWriterInsertErrorDropsBatch(t *testing.T) {
	p := NewPipeline(100)
	st := &failStore{fail: true, err: errors.New("db disk full")}
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "warn")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewWriter(p, st, WriterConfig{BatchSize: 2, FlushInterval: 20 * time.Millisecond}, logger).Run(ctx)
	}()

	// Submit 2 records -> should trigger batch flush and fail
	p.Submit(store.AuditRecord{RequestID: "req-1"})
	p.Submit(store.AuditRecord{RequestID: "req-2"})

	// Wait for the first batch to be attempted and fail
	deadline := time.Now().Add(2 * time.Second)
	for st.attemptCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// Now store allows subsequent writes
	st.setFail(false)
	p.Submit(store.AuditRecord{RequestID: "req-3"})
	p.Submit(store.AuditRecord{RequestID: "req-4"})

	for st.n() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	// Verify failed batch was discarded (not retried) and second batch succeeded
	if st.n() != 2 {
		t.Fatalf("written = %d, want 2 (first batch was dropped on failure)", st.n())
	}
	if !strings.Contains(buf.String(), "failed to insert audit records") {
		t.Fatalf("expected warning log about insert failure, got: %s", buf.String())
	}
}

func TestWriterLogsDroppedWarn(t *testing.T) {
	p := NewPipeline(2)
	// Fill buffer and drop 2 records
	for i := 0; i < 4; i++ {
		p.Submit(store.AuditRecord{RequestID: fmt.Sprint(i)})
	}
	if p.Dropped() != 2 {
		t.Fatalf("dropped = %d, want 2", p.Dropped())
	}

	st := &fakeStore{}
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "warn")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewWriter(p, st, WriterConfig{BatchSize: 10, FlushInterval: 20 * time.Millisecond}, logger).Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for st.n() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(buf.String(), "audit records dropped") {
		t.Fatalf("expected warning log about dropped records, got: %s", buf.String())
	}
}

type errorDeleter struct {
	err error
}

func (e *errorDeleter) DeleteAuditBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, e.err
}

func TestRetentionErrorLogsWarn(t *testing.T) {
	st := &errorDeleter{err: errors.New("delete failed")}
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "warn")

	ctx, cancel := context.WithCancel(context.Background())
	r := &Retention{
		St:            st,
		RetentionDays: 7,
		Interval:      time.Hour,
		Log:           logger,
	}

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "failed to delete expired audit records") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(buf.String(), "failed to delete expired audit records") {
		t.Fatalf("expected warning log about retention failure, got: %s", buf.String())
	}
}

func TestRetentionDefaults(t *testing.T) {
	st := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Retention{St: st} // all default/zero
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
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

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.before) == 0 {
		t.Fatal("expected at least 1 delete call")
	}
	// RetentionDays should default to 30
	approxWanted := time.Now().AddDate(0, 0, -30)
	diff := st.before[0].Sub(approxWanted)
	if diff < -time.Minute || diff > time.Minute {
		t.Fatalf("before[0] = %v, want approx %v", st.before[0], approxWanted)
	}
}

func TestPipelineAndWriterDefaults(t *testing.T) {
	p := NewPipeline(0)
	if cap(p.ch) != 8192 {
		t.Fatalf("capacity = %d, want 8192", cap(p.ch))
	}
	st := &fakeStore{}
	w := NewWriter(p, st, WriterConfig{}, nil)
	if w.cfg.BatchSize != 256 {
		t.Fatalf("BatchSize = %d, want 256", w.cfg.BatchSize)
	}
	if w.cfg.FlushInterval != 250*time.Millisecond {
		t.Fatalf("FlushInterval = %v, want 250ms", w.cfg.FlushInterval)
	}
	if w.log == nil {
		t.Fatal("log is nil")
	}
}
