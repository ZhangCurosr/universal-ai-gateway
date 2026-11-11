package core

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// CORE-12：真实的多步适配器已完成付费任务，但结果下载失败。
func TestPaidMultiStepFailureSettlesOnce(t *testing.T) {
	env := newTestEnv(t, 2, func(w http.ResponseWriter, _ int32, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"paid-task"}`)
		case r.URL.Path == "/v1/tasks/paid-task":
			_, _ = io.WriteString(w, `{"status":"completed","pages":7}`)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	route := env.core.routes["parser"]
	route.Upstream = mineru.Protocol
	route.Egress = mineru.Egress{InitialPollInterval: time.Nanosecond}
	env.core.routes["parser"] = route
	in := mineru.Ingress{}
	rt := in.Routes()[0]
	req := httptest.NewRequest(http.MethodPost, rt.Path, strings.NewReader("file"))
	req.Header.Set("X-UAIG-Model", "parser")
	rec := httptest.NewRecorder()
	env.core.Handler(in, rt).ServeHTTP(rec, env.withIdentity(req))
	if rec.Code != http.StatusBadGateway || env.calls.Load() != 3 {
		t.Fatalf("paid failure: status=%d calls=%d body=%s, want 502 and submit/poll/download only", rec.Code, env.calls.Load(), rec.Body.String())
	}
	ev := env.usageEvent(t)
	if ev.Usage["pages"] != 7 || ev.Usage["requests"] != 1 || ev.CostNanoUSD != 71_000_000 || ev.UsageSource != "upstream" {
		t.Fatalf("paid failure event = %+v, want 7 pages + 1 request", ev)
	}
	if len(env.ledger.Events()) != 0 {
		t.Fatal("paid failure generated duplicate billing events")
	}
	if s, _ := env.ledger.Snapshot(1); s.ReservedNanoUSD != 0 || s.SpentNanoUSD != ev.CostNanoUSD {
		t.Fatalf("paid failure account = %+v", s)
	}
	if a := env.lastAudit(t, 1); a.Attempts != 1 {
		t.Fatalf("paid failure audit = %+v, want one attempt", a)
	}
	assertNoLeases(t, env)
}

type hardeningEgress struct {
	protocol.Egress
	response *protocol.Response
	err      error
	cancel   context.CancelFunc
	released chan struct{}
}

func (e hardeningEgress) Do(_ context.Context, up protocol.Upstream, _ *protocol.Call) (*protocol.Response, error) {
	if e.released != nil {
		up.Client.(interface{ RetainResponseBuffer(func()) }).RetainResponseBuffer(func() { close(e.released) })
	}
	if e.cancel != nil {
		e.cancel()
	}
	return e.response, e.err
}

type hardeningMeter struct{}

func (hardeningMeter) Usage() (pricing.Usage, protocol.UsageSource) {
	return pricing.Usage{pricing.UnitInputTokens: 2, pricing.UnitOutputTokens: 1}, protocol.UsageUpstream
}
func (hardeningMeter) Fault() (protocol.Outcome, bool) { return protocol.Outcome{}, false }

func TestCanceledPaidAttemptStillSettles(t *testing.T) {
	env := newTestEnv(t, 2, func(http.ResponseWriter, int32, *http.Request) { t.Error("unexpected network call") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	route := env.core.routes["claude"]
	route.Egress = hardeningEgress{Egress: route.Egress, cancel: cancel, released: released,
		response: &protocol.Response{Body: []byte("partial must not be forwarded"), Meter: hardeningMeter{}},
		err:      &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeNetworkError}, Err: context.Canceled}}
	env.core.routes["claude"] = route
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	env.handler(t, anthropic.Protocol, anthropic.OpMessages).ServeHTTP(rec, env.withIdentity(req))
	select {
	case <-released:
	default:
		t.Fatal("canceled attempt did not release its response buffers on return")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("canceled paid attempt wrote %q", rec.Body.String())
	}
	if ev := env.usageEvent(t); ev.CostNanoUSD != 21_000 {
		t.Fatalf("canceled paid attempt event = %+v", ev)
	}
	if a := env.lastAudit(t, 1); a.Attempts != 1 || a.Status != statusClientClosed || a.Outcome != "canceled" {
		t.Fatalf("canceled paid attempt audit = %+v", a)
	}
	assertNoLeases(t, env)
}

// pipeResponseWriter 的对端不读取；真实 net.Conn deadline 必须打断 Write/Flush。
type pipeResponseWriter struct {
	header       http.Header
	conn         net.Conn
	buffer       bytes.Buffer
	bufferWrites bool
	deadlines    []time.Time
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }
func (w *pipeResponseWriter) WriteHeader(int)     {}
func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	if w.bufferWrites {
		return w.buffer.Write(p)
	}
	return w.conn.Write(p)
}
func (w *pipeResponseWriter) FlushError() error {
	_, err := w.conn.Write(w.buffer.Bytes())
	w.buffer.Reset()
	return err
}
func (w *pipeResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return w.conn.SetWriteDeadline(deadline)
}

type hardeningEvents struct{ done bool }

func (s *hardeningEvents) Next() (sse.Event, error) {
	if s.done {
		return sse.Event{}, io.EOF
	}
	s.done = true
	return sse.Event{Data: []byte("first")}, nil
}
func (*hardeningEvents) Close() error { return nil }

func TestSlowClientWritesReleaseResources(t *testing.T) {
	for _, tt := range []struct {
		name  string
		kind  protocol.ResponseKind
		flush bool
	}{
		{"buffered write", protocol.ResponseBuffered, false},
		{"buffered flush", protocol.ResponseBuffered, true},
		{"event flush", protocol.ResponseEvents, true},
		{"raw write", protocol.ResponseRaw, false},
		{"raw flush", protocol.ResponseRaw, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnvWith(t, 1, func(c *config.Config) {
				for i := range c.Pools {
					c.Pools[i].Timeouts.StreamIdle = 50 * time.Millisecond
				}
			}, func(http.ResponseWriter, int32, *http.Request) { t.Error("unexpected network call") })
			resp := &protocol.Response{Kind: tt.kind, Body: []byte("buffered"), Header: http.Header{}, Meter: hardeningMeter{}}
			if tt.kind == protocol.ResponseEvents {
				resp.Events = &hardeningEvents{}
			}
			if tt.kind == protocol.ResponseRaw {
				resp.Raw = io.NopCloser(strings.NewReader("raw"))
			}
			route := env.core.routes["claude"]
			route.Egress = hardeningEgress{Egress: route.Egress, response: resp}
			env.core.routes["claude"] = route
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			w := &pipeResponseWriter{header: http.Header{}, conn: server, bufferWrites: tt.flush}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
			h := env.handler(t, anthropic.Protocol, anthropic.OpMessages)
			done := make(chan struct{})
			go func() { defer close(done); h.ServeHTTP(w, env.withIdentity(req)) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("slow client kept the handler blocked after its write deadline")
			}
			if len(w.deadlines) < 2 || !w.deadlines[len(w.deadlines)-1].IsZero() {
				t.Fatalf("write deadlines = %v, want renewed deadlines and final clear", w.deadlines)
			}
			if s, _ := env.ledger.Snapshot(1); s.ReservedNanoUSD != 0 {
				t.Fatalf("slow client retained reservation: %+v", s)
			}
			env.usageEvent(t)
			if a := env.lastAudit(t, 1); a.Outcome != "canceled" {
				t.Fatalf("slow client audit = %+v, want canceled", a)
			}
			assertNoLeases(t, env)
		})
	}
}

func assertNoLeases(t *testing.T, env *testEnv) {
	t.Helper()
	for _, key := range env.core.d.Scheduler.KeyStatus() {
		if key.InFlight != 0 {
			t.Fatalf("key %d retained %d leases", key.ID, key.InFlight)
		}
	}
}
