package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

var pepper = []byte("test-pepper")

const (
	userToken     = auth.TokenPrefix + "user"
	disabledToken = auth.TokenPrefix + "disabled"
)

// newTestRouter 组装一个不带上游的路由：Proxy 为 nil，所以只能测试中间件、/v1/models、/healthz。
func newTestRouter(t *testing.T, rpm int) http.Handler {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools: [ { name: p, protocol: anthropic, base_url: "https://example.com" } ]
models:
  - { name: zeta, pool: p, upstream_model: z, max_output_tokens: 10 }
  - { name: alpha, pool: p, upstream_model: a, max_output_tokens: 10 }
`))
	if err != nil {
		t.Fatal(err)
	}
	ix := auth.NewIndex()
	ix.Replace([]store.Client{
		{ID: 1, TokenHash: auth.HashToken(pepper, userToken), Role: store.RoleUser, RPMLimit: rpm, AllowedModels: []string{"alpha"}},
		{ID: 2, TokenHash: auth.HashToken(pepper, disabledToken), Role: store.RoleUser, Disabled: true},
	})
	return NewDataRouter(DataDeps{Config: cfg, Index: ix, Pepper: pepper, Limiter: ratelimit.New(nil), IPLimiter: ratelimit.New(nil),
		Overload: NewOverloadGuard(16, 1<<30, 32<<20), Logger: obs.Discard()})
}

func serve(h http.Handler, method, path, token string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzNoAuth(t *testing.T) {
	t.Skip("TODO(T11)")
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/healthz", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUnauthenticatedAnthropicFormat(t *testing.T) {
	t.Skip("TODO(T11)")
	rec := serve(newTestRouter(t, 0), http.MethodPost, "/v1/messages", "", nil)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"authentication_error"`) || !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUnauthenticatedOpenAIFormat(t *testing.T) {
	t.Skip("TODO(T11)")
	rec := serve(newTestRouter(t, 0), http.MethodPost, "/v1/chat/completions", auth.TokenPrefix+"unknown", nil)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDisabledClient403(t *testing.T) {
	t.Skip("TODO(T11)")
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/v1/models", disabledToken, nil)
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRateLimited429RetryAfter(t *testing.T) {
	t.Skip("TODO(T11)")
	h := newTestRouter(t, 1)
	if rec := serve(h, http.MethodGet, "/v1/models", userToken, nil); rec.Code != 200 {
		t.Fatalf("first request status = %d", rec.Code)
	}
	rec := serve(h, http.MethodGet, "/v1/models", userToken, nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request status=%d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestModelsOpenAIFormat(t *testing.T) {
	t.Skip("TODO(T11)")
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/v1/models", userToken, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"object":"list"`) || !strings.Contains(body, `"id":"alpha"`) {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	if strings.Contains(body, "zeta") { // 白名单过滤
		t.Fatalf("model not in allow-list returned: %s", body)
	}
}

func TestModelsAnthropicFormat(t *testing.T) {
	t.Skip("TODO(T11)")
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/v1/models", userToken, map[string]string{"anthropic-version": "2023-06-01"})
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"type":"model"`) || !strings.Contains(body, `"first_id":"alpha"`) || !strings.Contains(body, `"has_more":false`) {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
}

func TestUnknownPath404(t *testing.T) {
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/nope", "", nil)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
