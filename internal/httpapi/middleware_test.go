package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
)

func TestRequestIDIgnoresClientValue(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = obs.RequestIDFrom(r.Context()) }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "client-chosen")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got := rec.Header().Get("X-Request-Id")
	if got == "client-chosen" || !strings.HasPrefix(got, "req_") || len(got) != 28 || seen != got {
		t.Fatalf("X-Request-Id = %q, ctx = %q", got, seen)
	}
}

func TestRecoverReturns500(t *testing.T) {
	h := Recover(obs.Discard())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"api_error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFormatForPath(t *testing.T) {
	if FormatForPath("/v1/messages") != apperr.FormatAnthropic || FormatForPath("/v1/chat/completions") != apperr.FormatOpenAI {
		t.Fatal("FormatForPath mismatch")
	}
}

// OVL-01 / OVL-02：并发上限
func TestOverloadGuardRejectsWhenFull(t *testing.T) {
	t.Skip("TODO(T11)")
	g := NewOverloadGuard(1, 1<<20, 1<<10)
	release := make(chan struct{})
	entered := make(chan struct{})
	h := g.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
		close(done)
	}()
	<-entered
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), "overloaded_error") {
		t.Fatalf("second request status=%d retry=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	close(release)
	<-done
	if n, b := g.InFlight(); n != 0 || b != 0 { // OVL-03：结束后归还
		t.Fatalf("InFlight() = %d, %d after completion, want 0, 0", n, b)
	}
}

// OVL-02：请求体字节预算；未知长度按单个请求体上限占用
func TestOverloadGuardBodyBudget(t *testing.T) {
	t.Skip("TODO(T11)")
	g := NewOverloadGuard(100, 1000, 600)
	var inside int64
	h := g.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { _, inside = g.InFlight() }))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("a", 300)))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if inside != 300 {
		t.Fatalf("reserved %d bytes for Content-Length 300, want 300", inside)
	}
	chunked := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("x"))
	chunked.ContentLength = -1
	h.ServeHTTP(httptest.NewRecorder(), chunked)
	if inside != 600 {
		t.Fatalf("reserved %d bytes for unknown length, want 600", inside)
	}
}

// OVL-04 / OVL-05：鉴权前按 IP 限流，不同 IP 互不影响
func TestIPRateLimit(t *testing.T) {
	t.Skip("TODO(T11)")
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		ClientIP(clientip.Trusted{}), IPRateLimit(ratelimit.New(nil), 1))
	send := func(remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if send("198.51.100.1:1") != 200 || send("198.51.100.1:2") != 429 || send("198.51.100.2:1") != 200 {
		t.Fatal("per-IP limiting incorrect")
	}
}
