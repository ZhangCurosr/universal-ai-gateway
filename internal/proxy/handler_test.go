package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider/anthropic"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider/openai"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
)

// testEnv 组装一个使用假上游的完整数据面（不含 httpapi 中间件）。
type testEnv struct {
	handler  *Handler
	ledger   *ledger.Memory
	upstream *httptest.Server
	calls    atomic.Int32
}

// newTestEnv 创建环境：一个 anthropic 池（模型 "claude"）与一个 openai_chat 池（模型 "gpt"），
// 每个池 nKeys 个 Key，全部指向 upstream。upstream 处理函数的第二个参数是第几次调用（从 1 开始）。
func newTestEnv(t *testing.T, nKeys int, upstream func(w http.ResponseWriter, call int32, r *http.Request)) *testEnv {
	t.Helper()
	env := &testEnv{}
	env.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream(w, env.calls.Add(1), r)
	}))
	t.Cleanup(env.upstream.Close)

	cfg, err := config.Parse([]byte(`
server:
  data_dir: ./data
  master_key_file: ./secrets/master.key
pools:
  - name: anth
    protocol: anthropic
    base_url: "` + env.upstream.URL + `"
  - name: oai
    protocol: openai_chat
    base_url: "` + env.upstream.URL + `"
models:
  - name: claude
    pool: anth
    upstream_model: claude-upstream
    max_output_tokens: 1000
    price: { input: 3, output: 15 }
  - name: gpt
    pool: oai
    upstream_model: gpt-upstream
    max_output_tokens: 1000
    price: { input: 1, output: 2 }
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	sched := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{Logger: obs.Discard()})
	var keys []*scheduler.Key
	for i := 0; i < nKeys; i++ {
		keys = append(keys, scheduler.NewKey(int64(i+1), "anth", "k", []byte("sk-a"), 1, 10, scheduler.PersistedReady))
		keys = append(keys, scheduler.NewKey(int64(100+i+1), "oai", "k", []byte("sk-o"), 1, 10, scheduler.PersistedReady))
	}
	sched.SetKeys(keys)
	env.ledger = ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: 100})
	env.ledger.LoadAccounts([]ledger.Account{{ClientID: 1, QuotaNanoUSD: 1_000_000_000_000}})
	env.handler = New(Deps{
		Config:     cfg,
		Ledger:     env.ledger,
		Scheduler:  sched,
		Adapters:   map[provider.Protocol]provider.Adapter{provider.ProtocolAnthropic: anthropic.Adapter{}, provider.ProtocolOpenAIChat: openai.Adapter{}},
		Audit:      audit.NewPipeline(100),
		HTTPClient: env.upstream.Client(),
		Logger:     obs.Discard(),
	})
	return env
}

// do 以客户端 1 的身份发送请求，返回响应。
func (env *testEnv) do(t *testing.T, proto provider.Protocol, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/chat/completions"
	if proto == provider.ProtocolAnthropic {
		path = "/v1/messages"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx := auth.WithPrincipal(req.Context(), &auth.Principal{ClientID: 1, Role: "user"})
	ctx = obs.WithRequestID(ctx, "req_test")
	rec := httptest.NewRecorder()
	env.handler.ForProtocol(proto).ServeHTTP(rec, req.WithContext(ctx))
	return rec
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
	rec := env.do(t, provider.ProtocolAnthropic, `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"text":"ok"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPassthroughStreamAnthropic(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 1, func(w http.ResponseWriter, _ int32, _ *http.Request) {
		sseWrite(w, strings.Split(anthropicStream, "|")...)
	})
	rec := env.do(t, provider.ProtocolAnthropic, `{"model":"claude","stream":true,"max_tokens":100,"messages":[]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	// LIFE-15：结算使用上游 usage（10 输入 × 3000 + 5 输出 × 15000 nano）
	select {
	case ev := <-env.ledger.Events():
		if ev.InputTokens != 10 || ev.OutputTokens != 5 || ev.CostNanoUSD != 10*3000+5*15000 || ev.UsageSource != "upstream" {
			t.Fatalf("usage event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no usage event")
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
	rec := env.do(t, provider.ProtocolAnthropic, `{"model":"claude","max_tokens":10,"messages":[]}`)
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
	rec := env.do(t, provider.ProtocolAnthropic, `{"model":"claude","stream":true,"max_tokens":100,"messages":[]}`)
	if env.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", env.calls.Load())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "text_delta") || !strings.Contains(body, "event: error") {
		t.Fatalf("client body = %s, want forwarded events followed by an error event", body)
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
	rec := env.do(t, provider.ProtocolAnthropic, `{"model":"claude","messages":[]}`)
	if rec.Code != 400 || env.calls.Load() != 1 || !strings.Contains(rec.Body.String(), "max_tokens: required") {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, env.calls.Load(), rec.Body.String())
	}
}

// LIFE-08：M1 不支持跨协议
func TestProtocolMismatchRejected(t *testing.T) {
	t.Skip("TODO(T10)")
	env := newTestEnv(t, 1, func(http.ResponseWriter, int32, *http.Request) { t.Error("upstream must not be called") })
	rec := env.do(t, provider.ProtocolOpenAIChat, `{"model":"claude","messages":[]}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
