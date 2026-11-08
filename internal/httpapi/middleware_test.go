package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// testErrorWriter 写 {"code","message"}，让中间件测试不依赖任何具体协议。
func testErrorWriter(w http.ResponseWriter, _ string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]string{"code": string(err.Code), "message": err.Message})
}

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
	h := Recover(obs.Discard(), testErrorWriter)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 未注入 Logger 时 Recover 不能在 defer 里二次 panic。
func TestRecoverNilLogger(t *testing.T) {
	h := Recover(nil, testErrorWriter)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rec.Code != 500 {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// OVL-01 / OVL-02：并发上限
func TestOverloadGuardRejectsWhenFull(t *testing.T) {
	g := NewOverloadGuard(1, 1<<20, 1<<10)
	release := make(chan struct{})
	entered := make(chan struct{})
	h := g.Middleware(testErrorWriter, true)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
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
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"code":"overloaded"`) {
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
	g := NewOverloadGuard(100, 1000, 600)
	var inside int64
	h := g.Middleware(testErrorWriter, true)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { _, inside = g.InFlight() }))

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
	// 流式请求体的路由不占请求体预算
	stream := g.Middleware(testErrorWriter, false)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { _, inside = g.InFlight() }))
	big := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("x"))
	big.ContentLength = 1 << 30
	stream.ServeHTTP(httptest.NewRecorder(), big)
	if inside != 0 {
		t.Fatalf("reserved %d bytes for a streamed body, want 0", inside)
	}
}

// OVL-04 / OVL-05：鉴权前按 IP 限流，不同 IP 互不影响
func TestIPRateLimit(t *testing.T) {
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		ClientIP(clientip.Trusted{}), IPRateLimit(ratelimit.New(nil), 1, testErrorWriter))
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

// OVL-05: 客户端 IP 为零值时以键 "ip:unknown" 计数
func TestIPRateLimitUnknownIP(t *testing.T) {
	limiter := ratelimit.New(nil)
	h := chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		IPRateLimit(limiter, 1, testErrorWriter),
	)
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(); code != 200 {
		t.Fatalf("first unknown IP status = %d, want 200", code)
	}
	if code := send(); code != 429 {
		t.Fatalf("second unknown IP status = %d, want 429", code)
	}
}

func TestAccessLogFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ix := auth.NewIndex()
	ix.Replace([]store.Client{
		{ID: 42, TokenHash: auth.HashToken(pepper, userToken), Role: store.RoleUser},
	})
	trusted, _ := clientip.ParseTrusted([]string{"10.0.0.0/8"})
	h := chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
		RequestID,
		ClientIP(trusted),
		AccessLog(logger),
		Authenticate(ix, pepper, testErrorWriter),
	)

	// 1. 已鉴权请求
	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var logMap map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logMap); err != nil {
		t.Fatalf("unmarshal access log: %v, raw=%s", err, buf.String())
	}
	reqID, ok := logMap["request_id"].(string)
	if !ok || !strings.HasPrefix(reqID, "req_") || len(reqID) != 28 {
		t.Errorf("request_id = %v", logMap["request_id"])
	}
	if status, ok := logMap["status"].(float64); !ok || int(status) != 200 {
		t.Errorf("status = %v, want 200", logMap["status"])
	}
	if clientID, ok := logMap["client_id"].(float64); !ok || int64(clientID) != 42 {
		t.Errorf("client_id = %v, want 42", logMap["client_id"])
	}
	if clientIP, ok := logMap["client_ip"].(string); !ok || clientIP != "192.0.2.1" {
		t.Errorf("client_ip = %v, want 192.0.2.1", logMap["client_ip"])
	}
	if method, ok := logMap["method"].(string); !ok || method != "POST" {
		t.Errorf("method = %v, want POST", logMap["method"])
	}
	if path, ok := logMap["path"].(string); !ok || path != "/v1/test" {
		t.Errorf("path = %v, want /v1/test", logMap["path"])
	}
	if strings.Contains(buf.String(), userToken) {
		t.Errorf("access log leaked plain token: %s", buf.String())
	}

	// 2. 未鉴权请求：client_id 为 0，status 为 401
	buf.Reset()
	reqUnauth := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{}`))
	reqUnauth.RemoteAddr = "192.0.2.2:5678"
	recUnauth := httptest.NewRecorder()
	h.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != 401 {
		t.Fatalf("unauth status = %d, want 401", recUnauth.Code)
	}

	var logMapUnauth map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logMapUnauth); err != nil {
		t.Fatalf("unmarshal unauth access log: %v", err)
	}
	if status, ok := logMapUnauth["status"].(float64); !ok || int(status) != 401 {
		t.Errorf("unauth status = %v, want 401", logMapUnauth["status"])
	}
	if clientID, ok := logMapUnauth["client_id"].(float64); !ok || int64(clientID) != 0 {
		t.Errorf("unauth client_id = %v, want 0", logMapUnauth["client_id"])
	}
	if clientIP, ok := logMapUnauth["client_ip"].(string); !ok || clientIP != "192.0.2.2" {
		t.Errorf("unauth client_ip = %v, want 192.0.2.2", logMapUnauth["client_ip"])
	}
}

func TestOverloadGuardConcurrent(t *testing.T) {
	const (
		maxReqs = 50
		total   = 200
	)
	g := NewOverloadGuard(maxReqs, 1<<30, 1<<20)

	var (
		wg          sync.WaitGroup
		maxObserved atomic.Int64
		accepted    atomic.Int64
		rejected    atomic.Int64
	)

	h := g.Middleware(testErrorWriter, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur, _ := g.InFlight()
		for {
			old := maxObserved.Load()
			if cur <= old {
				break
			}
			if maxObserved.CompareAndSwap(old, cur) {
				break
			}
		}
		if cur > maxReqs {
			t.Errorf("inFlight observed %d > max %d", cur, maxReqs)
		}
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	start := make(chan struct{})
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
			h.ServeHTTP(rec, req)
			switch rec.Code {
			case http.StatusOK:
				accepted.Add(1)
			case http.StatusServiceUnavailable:
				rejected.Add(1)
			default:
				t.Errorf("unexpected status: %d", rec.Code)
			}
		}()
	}

	close(start)
	wg.Wait()

	if max := maxObserved.Load(); max > maxReqs {
		t.Fatalf("max observed in-flight %d exceeded limit %d", max, maxReqs)
	}
	if accepted.Load() == 0 || rejected.Load() == 0 {
		t.Fatalf("expected both accepted and rejected requests, got accepted=%d rejected=%d", accepted.Load(), rejected.Load())
	}
	if n, b := g.InFlight(); n != 0 || b != 0 {
		t.Fatalf("InFlight() = %d, %d after all done, want 0, 0", n, b)
	}
}

func TestSpoofedXFFIgnored(t *testing.T) {
	limiter := ratelimit.New(nil)
	h := chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		ClientIP(clientip.Trusted{}),
		IPRateLimit(limiter, 1, testErrorWriter),
	)

	send := func(remoteAddr, xff string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// 请求 1 来自 198.51.100.1:1，携带伪造 XFF "203.0.113.1" -> 成功 (200)
	if code := send("198.51.100.1:1", "203.0.113.1"); code != 200 {
		t.Fatalf("first request status = %d, want 200", code)
	}

	// 请求 2 来自相同来源 IP 198.51.100.1:2，伪造不同的 XFF "203.0.113.2"。
	// 若信任 XFF 则会被当作新 IP 放行；但 Trusted 为空时忽略 XFF，按 198.51.100.1 限流 -> 429
	if code := send("198.51.100.1:2", "203.0.113.2"); code != 429 {
		t.Fatalf("second request with different spoofed XFF status = %d, want 429", code)
	}

	// 请求 3 来自不同来源 IP 198.51.100.2:1，携带与请求 1 相同的伪造 XFF -> 成功 (200)
	if code := send("198.51.100.2:1", "203.0.113.1"); code != 200 {
		t.Fatalf("third request from different remote IP status = %d, want 200", code)
	}
}
