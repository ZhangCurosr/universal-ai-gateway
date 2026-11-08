package openairesp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// EG-R01：各操作的上游端点（已实现，可直接运行）
func TestUpstreamEndpoint(t *testing.T) {
	tests := []struct {
		call         protocol.Call
		method, path string
	}{
		{protocol.Call{Op: OpCreate}, "POST", "/v1/responses"},
		{protocol.Call{Op: OpGet, Params: map[string]string{"id": "r1", "query": "include[]=a"}}, "GET", "/v1/responses/r1?include[]=a"},
		{protocol.Call{Op: OpDelete, Params: map[string]string{"id": "r1"}}, "DELETE", "/v1/responses/r1"},
		{protocol.Call{Op: OpCancel, Params: map[string]string{"id": "r1"}}, "POST", "/v1/responses/r1/cancel"},
		{protocol.Call{Op: OpInputItems, Params: map[string]string{"id": "r1"}}, "GET", "/v1/responses/r1/input_items"},
	}
	for _, tt := range tests {
		m, p, ok := upstreamEndpoint(&tt.call)
		if !ok || m != tt.method || p != tt.path {
			t.Errorf("upstreamEndpoint(%s) = %s %s %v", tt.call.Op, m, p, ok)
		}
	}
	if _, _, ok := upstreamEndpoint(&protocol.Call{Op: OpGet}); ok {
		t.Error("get without id accepted")
	}
}

// PT-R03
func TestPrepareBodyStoreFlag(t *testing.T) {
	for body, want := range map[string]bool{`{"model":"a","input":"x"}`: true, `{"model":"a","store":true}`: true, `{"model":"a","store":false}`: false} {
		if _, store, err := prepareBody([]byte(body), "b"); err != nil || store != want {
			t.Errorf("prepareBody(%s) store=%v err=%v, want %v", body, store, err, want)
		}
	}
}

// HDR-R01 ~ HDR-R03
func TestNewRequestHeaders(t *testing.T) {
	in := http.Header{"Authorization": {"Bearer uai_live_client"}, "Openai-Beta": {"responses=v1"}, "Cookie": {"a=b"}}
	req, err := Egress{}.newRequest(context.Background(), "GET", "https://api.example.com/v1/responses/r1", []byte("sk-up"), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer sk-up" || req.Header.Get("Cookie") != "" || req.Header.Get("OpenAI-Beta") != "responses=v1" {
		t.Fatalf("headers = %v", req.Header)
	}
	if req.Header.Get("Content-Type") != "" {
		t.Error("Content-Type set on a request without body")
	}
}

// USE-R01 ~ USE-R04、STR-R01
func TestMeterFailedAndCutStreams(t *testing.T) {
	m := &meter{billable: true}
	m.observeEvent(sse.Event{Name: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_1"}}`)})
	m.observeEvent(sse.Event{Name: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"12345678"}`)})
	if u, src := m.Usage(); src != protocol.UsageEstimated || u.Get(pricing.UnitOutputTokens) != 2 || m.responseID != "resp_1" {
		t.Fatalf("cut stream: %v %v id=%q", u, src, m.responseID)
	}
	m.observeEvent(sse.Event{Name: "response.failed", Data: []byte(`{"type":"response.failed","response":{"id":"resp_1","error":{"code":"rate_limit_exceeded","message":"x"},"usage":{"input_tokens":3,"output_tokens":0}}}`)})
	if o, ok := m.Fault(); !ok || o.Kind != protocol.OutcomeRateLimited {
		t.Fatalf("Fault() = %v %v", o, ok)
	}
	if u, src := m.Usage(); src != protocol.UsageUpstream || u.Get(pricing.UnitInputTokens) != 3 {
		t.Fatalf("failed response usage: %v %v", u, src)
	}
	if u, src := (&meter{}).Usage(); src != protocol.UsageUpstream || len(u) != 0 {
		t.Fatalf("non-billable op: %v %v", u, src)
	}
}

// TL-R01、TL-R02
func TestAskModelTool(t *testing.T) {
	tr, err := Ingress{}.ToolRequest(ToolAskModel, json.RawMessage(`{"model":"gpt","input":"hi","max_output_tokens":50}`))
	if err != nil || tr.Op != OpCreate {
		t.Fatalf("ToolRequest() = %+v, %v", tr, err)
	}
	var body map[string]any
	if json.Unmarshal(tr.Body, &body) != nil || body["store"] != false || body["model"] != "gpt" || body["stream"] != nil {
		t.Fatalf("tool body = %s", tr.Body)
	}
	for _, bad := range []string{`{"input":"hi"}`, `{"model":"gpt"}`, `{"model":"gpt","input":"hi","extra":1}`, `{"model":"gpt","input":"hi","max_output_tokens":0}`} {
		if _, err := (Ingress{}).ToolRequest(ToolAskModel, json.RawMessage(bad)); err == nil {
			t.Errorf("ToolRequest(%s) accepted", bad)
		}
	}
	res, err := Ingress{}.ToolResult(ToolAskModel, 200, nil, []byte(`{"output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"Hello"},{"type":"output_text","text":" world"}]}]}`))
	if err != nil || res.IsError || res.Content[0].Text != "Hello world" {
		t.Fatalf("ToolResult(ok) = %+v, %v", res, err)
	}
	res, err = Ingress{}.ToolResult(ToolAskModel, 429, nil, []byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	if err != nil || !res.IsError || res.Content[0].Text != "slow down" {
		t.Fatalf("ToolResult(error) = %+v, %v", res, err)
	}
}
