package mineru

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestEgressUnits(t *testing.T) {
	eg := Egress{}
	units := eg.Units()
	if len(units) != 2 || units[0] != UnitPages || units[1] != UnitRequests {
		t.Errorf("Units() = %v, want [%q, %q]", units, UnitPages, UnitRequests)
	}
}

func TestEgressPrepare_Validation(t *testing.T) {
	eg := Egress{}

	// Valid prepare
	call, err := eg.Prepare(&protocol.Envelope{
		Op:    OpParse,
		Model: ModelParse,
		Body:  []byte(`{"url":"https://example.com/doc.pdf"}`),
	}, "upstream-mineru")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if call.Model != "upstream-mineru" {
		t.Errorf("call.Model = %q, want 'upstream-mineru'", call.Model)
	}

	// Unsupported operation
	_, err = eg.Prepare(&protocol.Envelope{
		Op: "unsupported",
	}, "upstream-mineru")
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("unsupported op error = %v, want invalid_request", err)
	}

	// OpTaskGet without id
	_, err = eg.Prepare(&protocol.Envelope{
		Op:     OpTaskGet,
		Params: map[string]string{},
	}, "upstream-mineru")
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("task.get without id error = %v, want invalid_request", err)
	}

	// OpTaskResult without id
	_, err = eg.Prepare(&protocol.Envelope{
		Op:     OpTaskResult,
		Params: map[string]string{"id": "   "},
	}, "upstream-mineru")
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("task.result with empty id error = %v, want invalid_request", err)
	}

	// Request body is JSON array instead of object
	_, err = eg.Prepare(&protocol.Envelope{
		Op:   OpParse,
		Body: []byte(`[1, 2, 3]`),
	}, "upstream-mineru")
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("json array body error = %v, want invalid_request", err)
	}
}

func TestEgressDo_ContextCanceledDuringPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/tasks" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"task_poll_cancel","status":"pending"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// 轮询间隔为默认 1s；ctx 在 10ms 后取消，验证 Do 在 100ms 内返回 *AttemptError
	eg := Egress{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)

	start := time.Now()
	_, err := eg.Do(ctx, protocol.Upstream{
		BaseURL:          srv.URL,
		Secret:           []byte("test_secret"),
		Client:           srv.Client(),
		MaxBufferedBytes: 1 << 20,
	}, &protocol.Call{
		Op:    OpParse,
		Model: ModelParse,
	})
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("Do took %v, want < 100ms", elapsed)
	}

	var ae *protocol.AttemptError
	if !errors.As(err, &ae) {
		t.Fatalf("Do error = %v, want *protocol.AttemptError", err)
	}
	if ae.Outcome.Kind != protocol.OutcomeCanceled && ae.Outcome.Kind != protocol.OutcomeTimeout {
		t.Errorf("ae.Outcome.Kind = %s, want OutcomeCanceled or OutcomeTimeout", ae.Outcome.Kind)
	}
}

func TestEgressDo_TaskFailedStatusMapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/tasks" {
			_, _ = w.Write([]byte(`{"id":"task_fail_1","status":"pending"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/task_fail_1" {
			_, _ = w.Write([]byte(`{"id":"task_fail_1","status":"failed","error":"file corrupted or unparseable"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	eg := Egress{
		InitialPollInterval: 5 * time.Millisecond,
		MaxPollInterval:     10 * time.Millisecond,
	}

	_, err := eg.Do(context.Background(), protocol.Upstream{
		BaseURL:          srv.URL,
		Secret:           []byte("test_secret"),
		Client:           srv.Client(),
		MaxBufferedBytes: 1 << 20,
	}, &protocol.Call{
		Op:    OpParse,
		Model: ModelParse,
	})

	var ae *protocol.AttemptError
	if !errors.As(err, &ae) {
		t.Fatalf("Do error = %v, want *protocol.AttemptError", err)
	}
	if ae.Outcome.Kind != protocol.OutcomeClientError {
		t.Errorf("Outcome.Kind = %v, want OutcomeClientError", ae.Outcome.Kind)
	}
	if ae.Outcome.Status != http.StatusBadRequest {
		t.Errorf("Outcome.Status = %d, want 400", ae.Outcome.Status)
	}
	if !strings.Contains(string(ae.Body), "file corrupted or unparseable") {
		t.Errorf("ae.Body = %s, want upstream error body", string(ae.Body))
	}
	if !strings.Contains(ae.Err.Error(), "file corrupted or unparseable") {
		t.Errorf("ae.Err = %v, want upstream error reason", ae.Err)
	}
}

func TestEgressDo_AuthAndNoLeakedHeaders(t *testing.T) {
	var (
		receivedAuth string
		receivedBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"task_created_101","status":"pending"}`))
	}))
	defer srv.Close()

	eg := Egress{UserAgent: "custom-mineru-ua"}
	call := &protocol.Call{
		Op:     OpTaskCreate,
		Model:  ModelParse,
		Body:   []byte(`{"url":"https://example.com/test.pdf"}`),
		Header: http.Header{"Authorization": {"Bearer client-token-must-not-leak"}},
		State:  pricing.Usage{UnitPages: 5, UnitRequests: 1},
	}

	resp, err := eg.Do(context.Background(), protocol.Upstream{
		BaseURL:          srv.URL,
		Secret:           []byte("upstream-secret-key-xyz"),
		Client:           srv.Client(),
		MaxBufferedBytes: 1 << 20,
	}, call)
	if err != nil {
		t.Fatalf("Do error = %v", err)
	}

	if receivedAuth != "Bearer upstream-secret-key-xyz" {
		t.Errorf("Authorization = %q, want Bearer upstream-secret-key-xyz", receivedAuth)
	}
	if strings.Contains(string(receivedBody), "client-token-must-not-leak") {
		t.Errorf("client token leaked into upstream body: %s", string(receivedBody))
	}
	if len(resp.Pins) != 1 || resp.Pins[0] != "task_created_101" {
		t.Errorf("resp.Pins = %v, want ['task_created_101']", resp.Pins)
	}
}

func TestEgressDo_UnsupportedOp(t *testing.T) {
	eg := Egress{}
	_, err := eg.Do(context.Background(), protocol.Upstream{}, &protocol.Call{Op: "invalid"})
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("unsupported op error = %v, want invalid_request", err)
	}
}

// mineruStub 返回一个模拟 MinerU：提交返回 task_1，轮询按 poll 应答，结果返回 Markdown。
func mineruStub(t *testing.T, poll http.HandlerFunc) (*httptest.Server, protocol.Upstream) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"task_1","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/task_1":
			poll(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/task_1/result":
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte("# doc"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, protocol.Upstream{BaseURL: srv.URL, Secret: []byte("test_secret"), Client: srv.Client(), MaxBufferedBytes: 1 << 20}
}

var fastPoll = Egress{InitialPollInterval: time.Millisecond, MaxPollInterval: 2 * time.Millisecond}

// spec/08 §8.2 + spec/06 §3.3：上游完成但没报告页数时不能按 0 页结算，交给内核按预留结算。
func TestEgressDo_NoPageCountFallsBackToReserve(t *testing.T) {
	_, up := mineruStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"task_1","status":"completed"}`))
	})
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Model: ModelParse})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if _, src := resp.Meter.Usage(); src != protocol.UsageNone {
		t.Errorf("usage source = %v, want UsageNone", src)
	}
}

func TestEgressDo_PageCountBilledFromUpstream(t *testing.T) {
	_, up := mineruStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"task_1","status":"completed","pages":7}`))
	})
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Model: ModelParse})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	u, src := resp.Meter.Usage()
	if src != protocol.UsageUpstream || u[UnitPages] != 7 {
		t.Errorf("usage = %v (%v), want pages=7 upstream", u, src)
	}
}

// SPEC-GAP(T31)：任务提交后轮询失败不得重试，否则会重复上传、产生第二个付费任务。
func TestEgressDo_FailureAfterSubmitNotRetryable(t *testing.T) {
	_, up := mineruStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Model: ModelParse})
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) {
		t.Fatalf("Do() error = %v, want *protocol.AttemptError", err)
	}
	if ae.Outcome.Kind.Retryable() {
		t.Errorf("Outcome.Kind = %v, want non-retryable", ae.Outcome.Kind)
	}
	if !strings.Contains(ae.Err.Error(), "task_1") {
		t.Errorf("error %q should carry the task id", ae.Err)
	}
}
