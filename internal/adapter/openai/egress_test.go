package openai

import (
	"context"
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// PT-03
func TestPrepareBodyInjectsUsage(t *testing.T) {
	t.Skip("TODO(T09B)")
	tests := []struct {
		name         string
		body         string
		wantInjected bool
	}{
		{"no stream_options", `{"model":"a","stream":true,"messages":[]}`, true},
		{"include_usage false", `{"model":"a","stream":true,"stream_options":{"include_usage":false,"x":1},"messages":[]}`, true},
		{"already true", `{"model":"a","stream":true,"stream_options":{"include_usage":true},"messages":[]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, injected, err := prepareBody([]byte(tt.body), "deepseek-chat", true)
			if err != nil {
				t.Fatal(err)
			}
			if injected != tt.wantInjected {
				t.Fatalf("injected = %v, want %v", injected, tt.wantInjected)
			}
			if v, ok := jsonx.GetBool(out, "stream_options", "include_usage"); !ok || !v {
				t.Fatalf("include_usage not true in %s", out)
			}
			if m, _ := jsonx.GetString(out, "model"); m != "deepseek-chat" {
				t.Fatalf("model = %q", m)
			}
		})
	}
	// 原 stream_options 中的其他字段必须保留
	out, _, _ := prepareBody([]byte(tests[1].body), "m", true)
	if v, ok := jsonx.GetInt64(out, "stream_options", "x"); !ok || v != 1 {
		t.Fatalf("stream_options.x lost: %s", out)
	}
}

func TestPrepareBodyNonStreamNoInject(t *testing.T) {
	t.Skip("TODO(T09B)")
	out, injected, err := prepareBody([]byte(`{"model":"a","messages":[]}`), "b", false)
	if err != nil || injected {
		t.Fatalf("injected=%v err=%v", injected, err)
	}
	if _, ok := jsonx.Get(out, "stream_options"); ok {
		t.Fatalf("stream_options added to non-stream request: %s", out)
	}
}

// HDR-02、HDR-04
func TestNewRequestHeaders(t *testing.T) {
	t.Skip("TODO(T09B)")
	in := http.Header{}
	in.Set("Authorization", "Bearer uai_live_client")
	in.Set("OpenAI-Organization", "org-x")
	req, err := Egress{}.newRequest(context.Background(), "https://api.deepseek.com", []byte("sk-up"), []byte(`{}`), in)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://api.deepseek.com/v1/chat/completions" {
		t.Fatalf("URL = %s", req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-up" {
		t.Fatalf("Authorization = %q, want upstream secret", got)
	}
	if req.Header.Get("OpenAI-Organization") != "" {
		t.Fatal("client header forwarded")
	}
}

// USE-03、USE-04
func TestUsageCachedTokens(t *testing.T) {
	t.Skip("TODO(T09B)")
	tests := []struct {
		name string
		body string
		want pricing.Usage
	}{
		{"openai", `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":60}}}`,
			pricing.Usage{pricing.UnitInputTokens: 40, pricing.UnitOutputTokens: 20, pricing.UnitCacheReadTokens: 60}},
		{"deepseek", `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}}`,
			pricing.Usage{pricing.UnitInputTokens: 70, pricing.UnitOutputTokens: 20, pricing.UnitCacheReadTokens: 30}},
		{"no cache", `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`, pricing.Usage{pricing.UnitInputTokens: 10, pricing.UnitOutputTokens: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tap := &meter{}
			tap.observeBody([]byte(tt.body))
			got, src := tap.Usage()
			if src != protocol.UsageUpstream || !got.Equal(tt.want) {
				t.Fatalf("Usage() = %v, %v; want %v", got, src, tt.want)
			}
		})
	}
}

// USE-05、USE-06：流式，最后一个 chunk 带 usage
func TestUsageStream(t *testing.T) {
	t.Skip("TODO(T09B)")
	tap := &meter{}
	for _, d := range []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`,
		`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}`,
		`[DONE]`,
	} {
		tap.observeEvent(sse.Event{Data: []byte(d)})
	}
	if got, src := tap.Usage(); src != protocol.UsageUpstream || !got.Equal(pricing.Usage{pricing.UnitInputTokens: 9, pricing.UnitOutputTokens: 3}) {
		t.Fatalf("Usage() = %v, %v", got, src)
	}
}

// USE-07：没有 usage 时按已观察到的文本字节估算输出
func TestMeterEstimatesOutputWithoutUsage(t *testing.T) {
	t.Skip("TODO(T09B)")
	tap := &meter{}
	tap.observeEvent(sse.Event{Data: []byte(`{"choices":[{"index":0,"delta":{"content":"12345"}}],"usage":null}`)})
	if got, src := tap.Usage(); src != protocol.UsageEstimated || got.Get(pricing.UnitOutputTokens) != 2 {
		t.Fatalf("Usage() = %v, %v; want output 2 (ceil(5/4)), estimated", got, src)
	}
}

// FWD-02、FWD-03
func TestShouldForwardFiltersInjectedUsageChunk(t *testing.T) {
	t.Skip("TODO(T09B)")
	usageChunk := sse.Event{Data: []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)}
	normal := sse.Event{Data: []byte(`{"id":"x","choices":[{"index":0,"delta":{"content":"a"}}],"usage":null}`)}
	done := sse.Event{Data: []byte(`[DONE]`)}
	if shouldForward(usageChunk, true) {
		t.Error("usage-only chunk forwarded although usage was injected by gateway")
	}
	if !shouldForward(usageChunk, false) {
		t.Error("usage-only chunk dropped although client requested usage")
	}
	if !shouldForward(normal, true) || !shouldForward(done, true) {
		t.Error("normal chunk or [DONE] dropped")
	}
}
