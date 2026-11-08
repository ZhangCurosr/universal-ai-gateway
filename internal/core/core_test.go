package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// testEnv 组装一个使用假上游的完整数据面（不含 httpapi 中间件）。
// 测试文件可以导入具体适配器（archtest 只约束非测试代码）；内核代码本身不可以。
type testEnv struct {
	core     *Core
	registry *protocol.Registry
	ledger   *ledger.Memory
	upstream *httptest.Server
	calls    atomic.Int32
}

const testConfig = `
server:
  data_dir: ./data
  master_key_file: ./secrets/master.key
pools:
  - name: anth
    protocol: anthropic
    base_url: "UPSTREAM"
  - name: oai
    protocol: openai_chat
    base_url: "UPSTREAM"
  - name: jobs
    protocol: jobs
    base_url: "UPSTREAM"
models:
  - name: claude
    pool: anth
    upstream_model: claude-upstream
    price: { input_tokens: "3/1M", output_tokens: "15/1M", cache_read_tokens: "0", cache_write_tokens: "0" }
    reserve: { output_tokens: 1000 }
  - name: gpt
    pool: oai
    upstream_model: gpt-upstream
    price: { input_tokens: "1/1M", output_tokens: "2/1M", cache_read_tokens: "0" }
    reserve: { output_tokens: 1000 }
  - name: parser
    pool: jobs
    price: { pages: "0.01", requests: "0.001" }
    reserve: { pages: 100 }
`

// newTestEnv 创建环境：anthropic 池（服务 "claude"）、openai_chat 池（"gpt"）、测试用非大模型协议 jobs 的池（"parser"），
// 每个池 nKeys 个 Key，全部指向 upstream。jobs 池第 i 个 Key 的凭据是 "sk-j<i>"。
// upstream 处理函数的第二个参数是第几次调用（从 1 开始）。
func newTestEnv(t *testing.T, nKeys int, upstream func(w http.ResponseWriter, call int32, r *http.Request)) *testEnv {
	t.Helper()
	env := &testEnv{}
	env.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream(w, env.calls.Add(1), r)
	}))
	t.Cleanup(env.upstream.Close)

	cfg, err := config.Parse([]byte(strings.ReplaceAll(testConfig, "UPSTREAM", env.upstream.URL)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	sched := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{Logger: obs.Discard()})
	var keys []*scheduler.Key
	for i := 0; i < nKeys; i++ {
		keys = append(keys,
			scheduler.NewKey(int64(i+1), "anth", "k", []byte("sk-a"), 1, 10, scheduler.PersistedReady),
			scheduler.NewKey(int64(100+i+1), "oai", "k", []byte("sk-o"), 1, 10, scheduler.PersistedReady),
			scheduler.NewKey(int64(200+i+1), "jobs", "k", []byte("sk-j"+string(rune('0'+i))), 1, 10, scheduler.PersistedReady))
	}
	sched.SetKeys(keys)
	env.ledger = ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: 100})
	env.ledger.LoadAccounts([]ledger.Account{{ClientID: 1, QuotaNanoUSD: 1_000_000_000_000}})
	env.registry = newRegistry(t)
	env.core, err = New(Deps{
		Config:     cfg,
		Registry:   env.registry,
		Ledger:     env.ledger,
		Scheduler:  sched,
		Audit:      audit.NewPipeline(100),
		HTTPClient: env.upstream.Client(),
		Logger:     obs.Discard(),
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	return env
}

// newRegistry 注册真实的 anthropic、openai 适配器与测试用 jobs 适配器；extra 可再注册转换器等。
func newRegistry(t *testing.T, extra ...func(*protocol.Registry) error) *protocol.Registry {
	t.Helper()
	reg := protocol.NewRegistry()
	base := []func(*protocol.Registry) error{anthropic.Register, openai.Register, registerJobs}
	for _, f := range append(base, extra...) {
		if err := f(reg); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Seal(); err != nil {
		t.Fatal(err)
	}
	return reg
}

// serve 以客户端 1 的身份，把请求经由与生产相同的 ServeMux 模式交给 Core.Handler(in, rt)。
func (env *testEnv) serve(t *testing.T, proto protocol.Protocol, op, method, path string, body io.Reader, h http.Header) *httptest.ResponseRecorder {
	t.Helper()
	in, ok := env.registry.Ingress(proto)
	if !ok {
		t.Fatalf("no ingress %q", proto)
	}
	mux := http.NewServeMux()
	for _, rt := range in.Routes() {
		if rt.Kind == protocol.RouteCall && rt.Op == op {
			mux.Handle(rt.Method+" "+rt.Path, env.core.Handler(in, rt))
		}
	}
	req := httptest.NewRequest(method, path, body)
	for k, vs := range h {
		req.Header[k] = vs
	}
	ctx := auth.WithPrincipal(req.Context(), &auth.Principal{ClientID: 1, Role: "user"})
	ctx = obs.WithRequestID(ctx, "req_test")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// do 发送一个大模型调用（JSON 请求体）。
func (env *testEnv) do(t *testing.T, proto protocol.Protocol, body string) *httptest.ResponseRecorder {
	t.Helper()
	switch proto {
	case anthropic.Protocol:
		return env.serve(t, proto, anthropic.OpMessages, http.MethodPost, "/v1/messages", strings.NewReader(body), nil)
	default:
		return env.serve(t, proto, openai.OpChatCompletions, http.MethodPost, "/v1/chat/completions", strings.NewReader(body), nil)
	}
}

func (env *testEnv) usageEvent(t *testing.T) store.UsageEvent {
	t.Helper()
	select {
	case ev := <-env.ledger.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no usage event")
		return store.UsageEvent{}
	}
}

func sseWrite(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		_, _ = io.WriteString(w, e+"\n\n")
		w.(http.Flusher).Flush()
	}
}

const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}|event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}|event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":5}}|event: message_stop
data: {"type":"message_stop"}`

// ---------------------------------------------------------------- 大模型路径

func TestPassthroughNonStream(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 1, func(w http.ResponseWriter, _ int32, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"model":"claude-upstream"`) {
			t.Errorf("upstream got body %s, want rewritten model", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	})
	rec := env.do(t, anthropic.Protocol, `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"text":"ok"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-Id") != "req_test" {
		t.Errorf("X-Request-Id = %q", rec.Header().Get("X-Request-Id"))
	}
}

func TestPassthroughStreamAnthropic(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 1, func(w http.ResponseWriter, _ int32, _ *http.Request) {
		sseWrite(w, strings.Split(anthropicStream, "|")...)
	})
	rec := env.do(t, anthropic.Protocol, `{"model":"claude","stream":true,"max_tokens":100,"messages":[]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	// LIFE-15：结算使用上游 usage（10 输入 × 3000 + 5 输出 × 15000 nano）
	ev := env.usageEvent(t)
	if ev.Usage[pricing.UnitInputTokens] != 10 || ev.Usage[pricing.UnitOutputTokens] != 5 ||
		ev.CostNanoUSD != 10*3000+5*15000 || ev.UsageSource != store.UsageSourceUpstream || ev.Op != anthropic.OpMessages {
		t.Fatalf("usage event = %+v", ev)
	}
}

// RTY-01 / RTY-02
func TestRetryOn429ThenSuccess(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 2, func(w http.ResponseWriter, call int32, _ *http.Request) {
		if call == 1 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_2","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	rec := env.do(t, anthropic.Protocol, `{"model":"claude","max_tokens":10,"messages":[]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "msg_2") || env.calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, env.calls.Load(), rec.Body.String())
	}
}

// RTY-01：首字节之后不重试
func TestNoRetryAfterFirstByte(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 2, func(w http.ResponseWriter, _ int32, _ *http.Request) {
		parts := strings.Split(anthropicStream, "|")
		sseWrite(w, parts[0], parts[1])
		panic(http.ErrAbortHandler) // 模拟上游连接中断
	})
	rec := env.do(t, anthropic.Protocol, `{"model":"claude","stream":true,"max_tokens":100,"messages":[]}`)
	if env.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", env.calls.Load())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "text_delta") || !strings.Contains(body, "event: error") {
		t.Fatalf("client body = %s, want forwarded events followed by an error event", body)
	}
	// 只拿到 message_start：输入用上游值，输出由适配器按文本字节估算（USE-07）
	if ev := env.usageEvent(t); ev.UsageSource != store.UsageSourceEstimated || ev.Usage[pricing.UnitInputTokens] != 10 {
		t.Fatalf("usage event = %+v", ev)
	}
}

// ST-02：客户端错误原样返回、不重试
func TestClientErrorPassedThrough(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 2, func(w http.ResponseWriter, _ int32, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`)
	})
	rec := env.do(t, anthropic.Protocol, `{"model":"claude","messages":[]}`)
	if rec.Code != 400 || env.calls.Load() != 1 || !strings.Contains(rec.Body.String(), "max_tokens: required") {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, env.calls.Load(), rec.Body.String())
	}
}

// CORE-03：没有转换器时拒绝跨协议
func TestProtocolMismatchRejected(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 1, func(http.ResponseWriter, int32, *http.Request) { t.Error("upstream must not be called") })
	rec := env.do(t, openai.Protocol, `{"model":"claude","messages":[]}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------- 非大模型路径（测试协议 jobs）

// 非大模型 API：上传文件（流式请求体）→ 得到任务 ID 并固定 Key；按页计费。
func TestJobsSubmitStreamsBodyPinsAndBillsPages(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 2, func(w http.ResponseWriter, _ int32, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/jobs" || string(b) != "%PDF-1.7 fake file" {
			t.Errorf("upstream got %s %s", r.URL.Path, b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"job-1","pages":3}`)
	})
	rec := env.serve(t, "jobs", "submit", http.MethodPost, "/jobs", strings.NewReader("%PDF-1.7 fake file"),
		http.Header{"Content-Type": {"application/pdf"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "job-1") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	ev := env.usageEvent(t)
	// 3 页 × 0.01 + 1 次 × 0.001 = 0.031 USD
	if ev.Model != "parser" || ev.Op != "submit" || ev.Usage["pages"] != 3 || ev.CostNanoUSD != 31_000_000 {
		t.Fatalf("usage event = %+v", ev)
	}
}

// CORE-07：流式请求体只尝试一次，即使还有其他 Key
func TestJobsStreamBodyNotRetried(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 2, func(w http.ResponseWriter, _ int32, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	rec := env.serve(t, "jobs", "submit", http.MethodPost, "/jobs", strings.NewReader("file"), nil)
	if env.calls.Load() != 1 || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("calls=%d status=%d, want 1 call and 503", env.calls.Load(), rec.Code)
	}
}

// spec/07 §7.1：查询任务时必须使用创建它的 Key；未知任务 404
func TestJobsStrictPinRoutesToSameKey(t *testing.T) {
	t.Skip("TODO(T10)")
	var submitKey atomic.Value
	env := newTestEnv(t, 3, func(w http.ResponseWriter, _ int32, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			submitKey.Store(r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"id":"job-7","pages":1}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != submitKey.Load() {
			t.Errorf("poll used %q, want the submitting key %q", got, submitKey.Load())
		}
		_, _ = io.WriteString(w, `{"id":"job-7","state":"done"}`)
	})
	if rec := env.serve(t, "jobs", "submit", http.MethodPost, "/jobs", strings.NewReader("f"), nil); rec.Code != 200 {
		t.Fatalf("submit status=%d body=%s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 10; i++ {
		if rec := env.serve(t, "jobs", "get", http.MethodGet, "/jobs/job-7", nil, nil); rec.Code != 200 {
			t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if rec := env.serve(t, "jobs", "get", http.MethodGet, "/jobs/unknown", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job status = %d, want 404", rec.Code)
	}
}

// 原样字节流（文件下载）：边读边写，Content-Type 来自适配器
func TestJobsRawDownload(t *testing.T) {
	t.Skip("TODO(T10)")
	payload := strings.Repeat("0123456789", 100_000) // 1 MB
	env := newTestEnv(t, 1, func(w http.ResponseWriter, _ int32, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"job-9","pages":1}`)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = io.WriteString(w, payload)
	})
	env.serve(t, "jobs", "submit", http.MethodPost, "/jobs", strings.NewReader("f"), nil)
	env.usageEvent(t)
	rec := env.serve(t, "jobs", "file", http.MethodGet, "/jobs/job-9/file", nil, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" || rec.Body.String() != payload {
		t.Fatalf("status=%d type=%q len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}

// ---------------------------------------------------------------- 已可运行（不依赖 T10）

// CORE-01：池使用未注册的协议时 New 返回错误
func TestNewRejectsUnknownProtocol(t *testing.T) {
	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools: [ { name: g, protocol: gemini, base_url: "https://example.com" } ]
models: [ { name: m, pool: g } ]
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Deps{Config: cfg, Registry: newRegistry(t)}); err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("New() error = %v, want unknown protocol error", err)
	}
}

// CORE-05：价目表必须覆盖出站适配器声明的全部单位
func TestNewRejectsIncompletePrice(t *testing.T) {
	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools: [ { name: j, protocol: jobs, base_url: "https://example.com" } ]
models: [ { name: m, pool: j, price: { pages: "0.01" } } ]
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Deps{Config: cfg, Registry: newRegistry(t)}); err == nil || !strings.Contains(err.Error(), `"requests"`) {
		t.Fatalf("New() error = %v, want missing price for unit requests", err)
	}
}

func TestModelsFiltered(t *testing.T) {
	env := newTestEnv(t, 1, func(http.ResponseWriter, int32, *http.Request) {})
	got := env.core.Models(&auth.Principal{AllowedModels: []string{"gpt"}})
	if len(got) != 1 || got[0].ID != "gpt" || got[0].Protocol != openai.Protocol {
		t.Fatalf("Models() = %+v", got)
	}
	if all := env.core.Models(&auth.Principal{}); len(all) != 3 || all[0].ID != "claude" || all[2].ID != "parser" {
		t.Fatalf("Models(all) = %+v, want sorted [claude gpt parser]", all)
	}
}

// LIFE-07b / CORE-11（带服务名的分支已可运行；固定记录分支依赖 T08 的 PinnedPool，见 TestResolveRouteFromPin）
func TestResolveRoute(t *testing.T) {
	env := newTestEnv(t, 1, func(http.ResponseWriter, int32, *http.Request) {})
	p := &auth.Principal{ClientID: 1, AllowedModels: []string{"gpt"}}
	if r, err := env.core.resolveRoute(&protocol.Envelope{Model: "gpt"}, p, ""); err != nil || r.Model.Name != "gpt" {
		t.Fatalf("resolveRoute(gpt) = %v, %v", r.Model.Name, err)
	}
	for model, want := range map[string]apperr.Code{"nope": apperr.CodeInvalidRequest, "claude": apperr.CodePermissionDenied, "": apperr.CodeInvalidRequest} {
		if _, err := env.core.resolveRoute(&protocol.Envelope{Model: model}, p, ""); apperr.CodeOf(err) != want {
			t.Errorf("resolveRoute(%q) error = %v, want %s", model, err, want)
		}
	}
	strict := &protocol.Envelope{Affinity: protocol.AffinityHint{Source: protocol.AffinityPin, Value: []byte("x"), Strict: true}}
	if _, err := env.core.resolveRoute(strict, p, "c1:j:unknown"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Errorf("resolveRoute(strict, unknown pin) error = %v, want not_found", err)
	}
	if r := env.core.poolRoutes["jobs"]; r.Model.Name != "parser" {
		t.Errorf("poolRoutes[jobs] = %q", r.Model.Name)
	}
}

func TestBodyLimit(t *testing.T) {
	l := config.LimitsConfig{MaxRequestBodyBytes: 100, MaxStreamBodyBytes: 1000}
	tests := []struct {
		rt   protocol.Route
		want int64
	}{
		{protocol.Route{Body: protocol.BodyBuffered}, 100},
		{protocol.Route{Body: protocol.BodyBuffered, MaxBodyBytes: 50}, 50},
		{protocol.Route{Body: protocol.BodyBuffered, MaxBodyBytes: 500}, 100}, // 不能超过全局
		{protocol.Route{Body: protocol.BodyStream}, 1000},
		{protocol.Route{Body: protocol.BodyStream, MaxBodyBytes: 200}, 200},
		{protocol.Route{Body: protocol.BodyNone}, 0},
	}
	for _, tt := range tests {
		if got := bodyLimit(tt.rt, l); got != tt.want {
			t.Errorf("bodyLimit(%+v) = %d, want %d", tt.rt, got, tt.want)
		}
	}
}

func TestSettledUsage(t *testing.T) {
	reserved := pricing.Usage{pricing.UnitInputTokens: 100, pricing.UnitOutputTokens: 1000}
	u, src := settledUsage(reserved, pricing.Usage{pricing.UnitInputTokens: 7, pricing.UnitOutputTokens: 3}, protocol.UsageUpstream)
	if src != store.UsageSourceUpstream || !u.Equal(pricing.Usage{pricing.UnitInputTokens: 7, pricing.UnitOutputTokens: 3}) {
		t.Errorf("upstream: %v %s", u, src)
	}
	u, src = settledUsage(reserved, pricing.Usage{pricing.UnitOutputTokens: 3}, protocol.UsageEstimated)
	if src != store.UsageSourceEstimated || !u.Equal(pricing.Usage{pricing.UnitInputTokens: 100, pricing.UnitOutputTokens: 3}) {
		t.Errorf("estimated: %v %s", u, src)
	}
	u, src = settledUsage(reserved, nil, protocol.UsageNone)
	if src != store.UsageSourceEstimated || !u.Equal(reserved) {
		t.Errorf("none: %v %s", u, src)
	}
}

func TestHeaderTimeout(t *testing.T) {
	to := config.TimeoutsConfig{ResponseHeaderBase: 60 * time.Second, ResponseHeaderPerMiB: 90 * time.Second}
	for _, tt := range []struct {
		n    int64
		want time.Duration
	}{{0, 60 * time.Second}, {1, 150 * time.Second}, {1 << 20, 150 * time.Second}, {1<<20 + 1, 240 * time.Second}} {
		if got := headerTimeout(to, tt.n); got != tt.want {
			t.Errorf("headerTimeout(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------- 转译路径

// fakeTranslator 把 OpenAI 入站请求「转换」为 Anthropic 上游请求，用于验证内核的转译路径（CORE-04）。
type fakeTranslator struct{ requestCalled atomic.Bool }

func (*fakeTranslator) From() protocol.Protocol { return openai.Protocol }
func (*fakeTranslator) To() protocol.Protocol   { return anthropic.Protocol }
func (f *fakeTranslator) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	f.requestCalled.Store(true)
	return &protocol.Call{Op: anthropic.OpMessages, Model: upstreamModel, Header: env.Header,
		Body: []byte(`{"model":"` + upstreamModel + `","max_tokens":10,"messages":[]}`)}, nil
}
func (*fakeTranslator) Response(_ *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	out := *resp
	out.Body = []byte(`{"translated":true}`)
	return &out, nil
}

// CORE-04：入站协议 ≠ 上游协议且注册了转换器时，内核走转译路径；用量仍取自上游响应
func TestTranslatorPathUsed(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 1, func(w http.ResponseWriter, _ int32, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"model":"claude-upstream"`) {
			t.Errorf("upstream body = %s, want translated request", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	tr := &fakeTranslator{}
	env.registry = newRegistry(t, func(r *protocol.Registry) error { return r.RegisterTranslator(tr) })
	var err error
	if env.core, err = New(Deps{Config: env.core.d.Config, Registry: env.registry, Ledger: env.ledger, Scheduler: env.core.d.Scheduler,
		Audit: audit.NewPipeline(10), HTTPClient: env.upstream.Client(), Logger: obs.Discard()}); err != nil {
		t.Fatal(err)
	}
	rec := env.do(t, openai.Protocol, `{"model":"claude","messages":[]}`)
	if rec.Code != 200 || rec.Body.String() != `{"translated":true}` || !tr.requestCalled.Load() {
		t.Fatalf("status=%d body=%s requestCalled=%v", rec.Code, rec.Body.String(), tr.requestCalled.Load())
	}
	if ev := env.usageEvent(t); ev.Usage[pricing.UnitInputTokens] != 1 || ev.UsageSource != store.UsageSourceUpstream {
		t.Fatalf("usage event = %+v", ev)
	}
}

// ---------------------------------------------------------------- 测试协议 jobs
//
// jobs 是一个最小的「非大模型」协议，只用于证明内核不依赖大模型假设：
//   POST /jobs            上传文件（流式请求体），返回 {"id","pages"}，按页 + 按次计费，固定任务 ID
//   GET  /jobs/{id}       查询任务（无请求体），必须使用创建它的 Key
//   GET  /jobs/{id}/file  下载结果（原样字节流）

func registerJobs(r *protocol.Registry) error {
	if err := r.RegisterIngress(jobsIngress{}, protocol.IngressOptions{Priority: 90}); err != nil {
		return err
	}
	return r.RegisterEgress(jobsEgress{})
}

type jobsIngress struct{}

func (jobsIngress) Protocol() protocol.Protocol { return "jobs" }
func (jobsIngress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/jobs", Kind: protocol.RouteCall, Op: "submit", Body: protocol.BodyStream},
		{Method: http.MethodGet, Path: "/jobs/{id}", Kind: protocol.RouteCall, Op: "get", Body: protocol.BodyNone},
		{Method: http.MethodGet, Path: "/jobs/{id}/file", Kind: protocol.RouteCall, Op: "file", Body: protocol.BodyNone},
	}
}
func (jobsIngress) Claims(*http.Request) bool { return false }
func (jobsIngress) Parse(r *http.Request, rt protocol.Route, _ []byte) (*protocol.Envelope, error) {
	env := &protocol.Envelope{Protocol: "jobs", Op: rt.Op, Model: "parser", Header: r.Header}
	if rt.Op == "submit" {
		env.Estimate = pricing.Usage{pricing.UnitRequests: 1}
		return env, nil
	}
	id := r.PathValue("id")
	env.Params = map[string]string{"id": id}
	env.Estimate = pricing.Usage{"pages": 0, pricing.UnitRequests: 0} // 查询与下载确定不计费（IN-G04）
	env.Affinity = protocol.AffinityHint{Source: protocol.AffinityPin, Value: []byte(id), Strict: true}
	return env, nil
}
func (jobsIngress) WriteError(w http.ResponseWriter, _ string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]string{"error": err.Message})
}
func (jobsIngress) StreamError(err *apperr.Error) sse.Event {
	return sse.Event{Data: []byte(err.Message)}
}
func (jobsIngress) WriteCatalog(w http.ResponseWriter, _ []protocol.ModelInfo) {
	w.WriteHeader(http.StatusNotFound)
}

type jobsEgress struct{}

func (jobsEgress) Protocol() protocol.Protocol { return "jobs" }
func (jobsEgress) Units() []string             { return []string{"pages", pricing.UnitRequests} }
func (jobsEgress) Prepare(env *protocol.Envelope, model string) (*protocol.Call, error) {
	return &protocol.Call{Op: env.Op, Model: model, Params: env.Params, BodyStream: env.BodyStream, Header: env.Header}, nil
}
func (jobsEgress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	method, path := http.MethodGet, "/jobs/"+call.Params["id"]
	switch call.Op {
	case "submit":
		method, path = http.MethodPost, "/jobs"
	case "file":
		path += "/file"
	}
	req, err := http.NewRequestWithContext(ctx, method, protocol.JoinURL(up.BaseURL, path), call.BodyStream)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(up.Secret))
	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type")
	if call.Op == "file" {
		return &protocol.Response{Kind: protocol.ResponseRaw, Header: h, Raw: resp.Body, Meter: jobsMeter{}}, nil
	}
	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}
	out := &protocol.Response{Kind: protocol.ResponseBuffered, Header: h, Body: body, Meter: jobsMeter{}}
	if call.Op == "submit" {
		var v struct {
			ID    string `json:"id"`
			Pages int64  `json:"pages"`
		}
		_ = json.Unmarshal(body, &v) // 测试协议：解析失败时用量为空
		out.Pins = []string{v.ID}
		out.Meter = jobsMeter{u: pricing.Usage{"pages": v.Pages, pricing.UnitRequests: 1}}
	}
	return out, nil
}

type jobsMeter struct{ u pricing.Usage }

func (m jobsMeter) Usage() (pricing.Usage, protocol.UsageSource) { return m.u, protocol.UsageUpstream }
func (jobsMeter) Fault() (protocol.Outcome, bool)                { return protocol.Outcome{}, false }
