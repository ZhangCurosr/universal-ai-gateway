package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/core"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

var pepper = []byte("test-pepper")

const (
	userToken     = auth.TokenPrefix + "user"
	disabledToken = auth.TokenPrefix + "disabled"
)

// newTestRouter 组装一个不带上游的路由：内核只用于模型列表，所以只能测试中间件、/v1/models、/healthz。
// 注册真实的 anthropic 与 openai 入站适配器（测试文件可以导入具体适配器）。
func newTestRouter(t *testing.T, rpm int) http.Handler {
	t.Helper()
	return NewDataRouter(testDataDeps(t, rpm))
}

// testDataDeps 组装 newTestRouter 使用的依赖，供需要额外字段（如 MCP）的测试修改后再构造路由。
func testDataDeps(t *testing.T, rpm int) DataDeps {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools: [ { name: p, protocol: anthropic, base_url: "https://example.com" } ]
models:
  - { name: zeta, pool: p, upstream_model: z, price: &free { input_tokens: "0", output_tokens: "0", cache_read_tokens: "0", cache_write_tokens: "0" } }
  - { name: alpha, pool: p, upstream_model: a, price: *free }
`))
	if err != nil {
		t.Fatal(err)
	}
	ix := auth.NewIndex()
	ix.Replace([]store.Client{
		{ID: 1, TokenHash: auth.HashToken(pepper, userToken), Role: store.RoleUser, RPMLimit: rpm, AllowedModels: []string{"alpha"}},
		{ID: 2, TokenHash: auth.HashToken(pepper, disabledToken), Role: store.RoleUser, Disabled: true},
	})
	reg := protocol.NewRegistry()
	if err := anthropic.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := openai.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Seal(); err != nil {
		t.Fatal(err)
	}
	c, err := core.New(core.Deps{Config: cfg, Registry: reg, Logger: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	return DataDeps{Config: cfg, Core: c, Registry: reg, Index: ix, Pepper: pepper, Limiter: ratelimit.New(nil),
		IPLimiter: ratelimit.New(nil), Overload: NewOverloadGuard(16, 1<<30, 32<<20), Logger: obs.Discard()}
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
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/healthz", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUnauthenticatedAnthropicFormat(t *testing.T) {
	rec := serve(newTestRouter(t, 0), http.MethodPost, "/v1/messages", "", nil)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"authentication_error"`) || !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUnauthenticatedOpenAIFormat(t *testing.T) {
	rec := serve(newTestRouter(t, 0), http.MethodPost, "/v1/chat/completions", auth.TokenPrefix+"unknown", nil)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDisabledClient403(t *testing.T) {
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/v1/models", disabledToken, nil)
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRateLimited429RetryAfter(t *testing.T) {
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

// RTE-04：未知路径用兜底协议（OpenAI）格式返回 404
func TestUnknownPathUsesFallbackFormat(t *testing.T) {
	rec := serve(newTestRouter(t, 0), http.MethodGet, "/nope", "", nil)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// RTE-05：未配置 MCP 时 /mcp 不存在（已可运行）
func TestMCPNotMountedWhenNil(t *testing.T) {
	if rec := serve(newTestRouter(t, 0), http.MethodPost, "/mcp", userToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// RTE-05：/mcp 经过鉴权，错误格式用 MCPErrorWriter；鉴权后 Principal 可用
func TestMCPMountedBehindAuth(t *testing.T) {
	d := testDataDeps(t, 0)
	d.MCP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFrom(r.Context()); !ok || p.ClientID != 1 {
			t.Errorf("principal = %+v, %v", p, ok)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	d.MCPErrorWriter = func(w http.ResponseWriter, _ string, err *apperr.Error) {
		w.Header().Set("X-Mcp-Error", "1")
		apperr.WriteJSON(w, err, map[string]string{"m": err.Message})
	}
	h := NewDataRouter(d)
	if rec := serve(h, http.MethodPost, "/mcp", "", nil); rec.Code != 401 || rec.Header().Get("X-Mcp-Error") != "1" {
		t.Fatalf("no token: status=%d header=%v", rec.Code, rec.Header())
	}
	if rec := serve(h, http.MethodPost, "/mcp", userToken, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("with token: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
