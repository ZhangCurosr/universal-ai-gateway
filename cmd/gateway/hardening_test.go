package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// HDR-06：客户端不能自动把网关持有的凭据带到重定向目标。
func TestUpstreamRedirectsNeverFollowed(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var reached atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
			defer target.Close()
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/same-target" {
					reached.Add(1)
					return
				}
				location := target.URL + "/target"
				if r.URL.Path == "/same" {
					location = "/same-target"
				}
				w.Header().Set("Location", location)
				w.WriteHeader(status)
			}))
			defer origin.Close()
			cfg := config.Default()
			client := newUpstreamClient(&cfg)
			// 仅替换测试用 TLS transport；保留生产客户端的 CheckRedirect。
			client.Transport = origin.Client().Transport
			defer client.CloseIdleConnections()
			for _, path := range []string{"/same", "/cross-and-downgrade"} {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL+path, strings.NewReader("{}"))
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer dummy")
				req.Header.Set("X-Api-Key", "dummy")
				req.Header.Set("X-Goog-Api-Key", "dummy")
				resp, err := client.Do(req)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					cancel()
					t.Fatal(err)
				}
				cancel()
				if resp.StatusCode != status || reached.Load() != 0 {
					t.Fatalf("redirect: status=%d target calls=%d, want original %d and no second request", resp.StatusCode, reached.Load(), status)
				}
			}
		})
	}
}

// OVL-06：Body.Close 后结果仍被内核持有，预算须保留到整个尝试结束。
func TestBufferedResponseBudgetLifetime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 600))
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Limits.MaxBufferedBodyBytes = 1024
	client := newUpstreamClient(&cfg)
	defer client.CloseIdleConnections()
	budget := client.Transport.(*responseBudgetTransport).budget
	read := func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		return protocol.ReadBody(protocol.Upstream{MaxBufferedBytes: 4096}, resp)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	if body, err := read(ctx1); err != nil || len(body) != 600 {
		t.Fatalf("first buffered response = %d bytes, %v", len(body), err)
	}
	if budget.used.Load() != 600 {
		t.Fatalf("held budget = %d, want 600", budget.used.Load())
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if _, err := read(ctx2); apperr.CodeOf(err) != apperr.CodeOverloaded {
		t.Fatalf("second buffered response error = %v, want overloaded", err)
	}
	cancel1()
	cancel2()
	waitResponseBudget(t, budget, 0)
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	if _, err := read(ctx3); err != nil {
		t.Fatalf("read after budget release: %v", err)
	}
	cancel3()
	waitResponseBudget(t, budget, 0)
}

func waitResponseBudget(t *testing.T, budget *responseBudget, want int64) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for budget.used.Load() != want {
		select {
		case <-timer.C:
			t.Fatalf("response budget = %d, want %d after attempt cancellation", budget.used.Load(), want)
		default:
			runtime.Gosched()
		}
	}
}

func TestRawResponsesDoNotConsumeBufferedBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Limits.MaxBufferedBodyBytes = 1024
	client := newUpstreamClient(&cfg)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
	budget := client.Transport.(*responseBudgetTransport).budget
	if err != nil || n != 4096 || budget.used.Load() != 0 {
		t.Fatalf("raw response: bytes=%d error=%v budget=%d, want 4096, nil, 0", n, err, budget.used.Load())
	}
}

type budgetScopeClient struct {
	client  *http.Client
	release func()
}

func (c *budgetScopeClient) Do(r *http.Request) (*http.Response, error) { return c.client.Do(r) }
func (c *budgetScopeClient) RetainResponseBuffer(release func())        { c.release = release }

// OVL-06：父 context 的取消不代表结果已经释放；转译/写入的收尾仍可能持有它。
func TestCanceledParentDoesNotReleaseOwnedResponseBuffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "still held")
	}))
	defer srv.Close()
	cfg := config.Default()
	client := newUpstreamClient(&cfg)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := &budgetScopeClient{client: client}
	resp, err := owner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := protocol.ReadBody(protocol.Upstream{Client: owner}, resp)
	if err != nil {
		t.Fatal(err)
	}
	if owner.release == nil {
		t.Fatal("ReadBody did not retain response buffer in the attempt owner")
	}
	defer owner.release()
	cancel()
	budget := client.Transport.(*responseBudgetTransport).budget
	if budget.used.Load() != int64(len(body)) {
		t.Fatalf("parent cancellation released a live response: used=%d bytes=%d", budget.used.Load(), len(body))
	}
	owner.release()
	owner.release() // 清理的重复执行不能重复归还预算。
	if budget.used.Load() != 0 {
		t.Fatalf("attempt cleanup retained %d response bytes", budget.used.Load())
	}
}

func TestResponseBudgetConcurrentReadsAndCancellation(t *testing.T) {
	budget := &responseBudget{limit: 4096}
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			body := &responseBudgetBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("a", 600))), ctx: ctx, budget: budget}
			_, err := body.ReadBuffered(1024, nil)
			if err != nil && apperr.CodeOf(err) != apperr.CodeOverloaded {
				t.Errorf("ReadBuffered: %v", err)
			}
			if used := budget.used.Load(); used < 0 || used > budget.limit {
				t.Errorf("budget used = %d, limit=%d", used, budget.limit)
			}
			cancel()
			if err := body.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
	waitResponseBudget(t, budget, 0)
}
