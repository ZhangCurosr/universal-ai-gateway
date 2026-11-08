package anthropic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// PT-01、PT-04：只改 model，其余值的原始字节不变
func TestPrepareBodyRewritesModelOnly(t *testing.T) {
	t.Skip("TODO(T09A)")
	messages := `[ {"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]} ]`
	system := `[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}]`
	tools := `[{"name":"t","input_schema":{"type":"object"}}]`
	body := []byte(`{"model":"alias","max_tokens":10,"messages":` + messages + `,"system":` + system + `,"tools":` + tools + `}`)
	out, injected, err := Adapter{}.PrepareBody(body, "claude-sonnet-4-5", true)
	if err != nil || injected {
		t.Fatalf("PrepareBody() injected=%v err=%v", injected, err)
	}
	if m, _ := jsonx.GetString(out, "model"); m != "claude-sonnet-4-5" {
		t.Fatalf("model = %q", m)
	}
	for _, part := range []string{messages, system, tools} {
		if !bytes.Contains(out, []byte(part)) {
			t.Errorf("value bytes changed, missing %s in %s", part, out)
		}
	}
}

// PT-02
func TestPrepareBodyRejectsNonObject(t *testing.T) {
	t.Skip("TODO(T09A)")
	_, _, err := Adapter{}.PrepareBody([]byte(`[1,2]`), "m", false)
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Fatalf("PrepareBody(array) error = %v, want invalid_request", err)
	}
}

// HDR-01 ~ HDR-03、HDR-05
func TestNewRequestHeaders(t *testing.T) {
	t.Skip("TODO(T09A)")
	in := http.Header{}
	in.Set("Authorization", "Bearer uai_live_client")
	in.Set("x-api-key", "uai_live_client")
	in.Set("Cookie", "a=b")
	in.Add("anthropic-beta", "prompt-caching-2024-07-31")
	in.Add("anthropic-beta", "output-128k-2025-02-19")
	req, err := Adapter{UserAgent: "uaig/test"}.NewRequest(context.Background(), "https://api.example.com/", []byte("sk-ant-upstream"), []byte(`{}`), in)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.String() != "https://api.example.com/v1/messages" {
		t.Fatalf("request = %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-ant-upstream" {
		t.Errorf("x-api-key = %q, want upstream secret", got)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" { // HDR-02
		t.Errorf("client headers leaked: %v", req.Header)
	}
	if got := req.Header.Values("anthropic-beta"); len(got) != 2 {
		t.Errorf("anthropic-beta = %v, want 2 values", got)
	}
	if got := req.Header.Get("anthropic-version"); got != DefaultVersion {
		t.Errorf("anthropic-version = %q, want default", got)
	}
	if got := req.Header.Get("User-Agent"); got != "uaig/test" {
		t.Errorf("User-Agent = %q", got)
	}
	b, _ := io.ReadAll(req.Body)
	if string(b) != `{}` {
		t.Errorf("body = %q", b)
	}
}

// USE-01、USE-02
func TestUsageTapStream(t *testing.T) {
	t.Skip("TODO(T09A)")
	f, err := os.Open("testdata/stream_basic.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tap := Adapter{}.NewUsageTap()
	r := sse.NewReader(f)
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		tap.ObserveEvent(ev)
	}
	got, ok := tap.Usage()
	want := pricing.Usage{InputTokens: 25, OutputTokens: 15, CacheReadTokens: 2000, CacheWriteTokens: 100}
	if !ok || got != want {
		t.Fatalf("Usage() = %+v, %v; want %+v, true", got, ok, want)
	}
}

func TestUsageTapBody(t *testing.T) {
	t.Skip("TODO(T09A)")
	tap := Adapter{}.NewUsageTap()
	tap.ObserveBody([]byte(`{"id":"msg","usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5}}`))
	got, ok := tap.Usage()
	if !ok || got != (pricing.Usage{InputTokens: 3, OutputTokens: 4, CacheReadTokens: 5}) {
		t.Fatalf("Usage() = %+v, %v", got, ok)
	}
}

// STR-05
func TestClassifyStreamError(t *testing.T) {
	t.Skip("TODO(T09A)")
	tests := []struct {
		data string
		want provider.OutcomeKind
	}{
		{`{"type":"error","error":{"type":"overloaded_error","message":"x"}}`, provider.OutcomeOverloaded},
		{`{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`, provider.OutcomeRateLimited},
		{`{"type":"error","error":{"type":"api_error","message":"x"}}`, provider.OutcomeServerError},
	}
	for _, tt := range tests {
		o, ok := Adapter{}.ClassifyStreamError(sse.Event{Name: "error", Data: []byte(tt.data)})
		if !ok || o.Kind != tt.want {
			t.Errorf("ClassifyStreamError(%s) = %v, %v; want %v", tt.data, o.Kind, ok, tt.want)
		}
	}
	if _, ok := (Adapter{}).ClassifyStreamError(sse.Event{Name: "ping", Data: []byte(`{}`)}); ok {
		t.Error("ping classified as error")
	}
}
