package openai

import (
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// spec/08 §5：每个适配器都必须通过一致性测试套件。
func TestIngressConformance(t *testing.T) {
	t.Skip("TODO(T09B)")
	gen := []byte(`{"model":"suite-model","max_completion_tokens":512,"max_tokens":99,"messages":[{"role":"user","content":"hi"}]}`)
	adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{
		Valid: []adaptertest.IngressCase{
			{Name: "generate", Op: OpChatCompletions, Body: gen, WantEstimate: pricing.Usage{
				pricing.UnitInputTokens:  pricing.EstimateInputTokens(len(gen)),
				pricing.UnitOutputTokens: 512, // max_completion_tokens 优先于 max_tokens
			}},
			{Name: "stream", Op: OpChatCompletions, WantStream: true,
				Body: []byte(`{"model":"suite-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)},
		},
		Invalid: []adaptertest.IngressCase{
			{Name: "missing model", Op: OpChatCompletions, Body: []byte(`{"messages":[]}`)},
			{Name: "not an object", Op: OpChatCompletions, Body: []byte(`[1,2]`)},
			{Name: "broken json", Op: OpChatCompletions, Body: []byte(`{"model":`)},
		},
	})
}

func TestEgressConformance(t *testing.T) {
	t.Skip("TODO(T09B)")
	streamReq := []byte(`{"model":"alias","stream":true,"messages":[{"role":"user","content":"hi"}],"temperature":0.2}`)
	bodyReq := []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)
	checkBody := func(in []byte) func(*testing.T, *http.Request, []byte) {
		return func(t *testing.T, _ *http.Request, out []byte) {
			if m, _ := jsonx.GetString(out, "model"); m != adaptertest.SuiteUpstreamModel {
				t.Errorf("upstream model = %q", m)
			}
			adaptertest.AssertTopLevelPreserved(t, in, out, "model", "stream_options")
		}
	}
	upstreamBody := []byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`)
	adaptertest.RunEgress(t, Egress{}, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name:          "stream",
				Envelope:      protocol.Envelope{Op: OpChatCompletions, Model: "alias", Stream: true, Body: streamReq},
				Upstream:      adaptertest.ServeFile("text/event-stream", "testdata/stream_basic.sse"),
				CheckUpstream: checkBody(streamReq),
				WantKind:      protocol.ResponseEvents,
				WantUsage:     pricing.Usage{pricing.UnitInputTokens: 20, pricing.UnitOutputTokens: 7, pricing.UnitCacheReadTokens: 100},
				WantSource:    protocol.UsageUpstream,
			},
			{
				Name:          "buffered",
				Envelope:      protocol.Envelope{Op: OpChatCompletions, Model: "alias", Body: bodyReq},
				Upstream:      adaptertest.ServeBody("application/json", upstreamBody),
				CheckUpstream: checkBody(bodyReq),
				WantKind:      protocol.ResponseBuffered,
				WantUsage:     pricing.Usage{pricing.UnitInputTokens: 9, pricing.UnitOutputTokens: 2},
				WantSource:    protocol.UsageUpstream,
				WantClient:    upstreamBody,
			},
		},
		InvalidRequests: []protocol.Envelope{{Op: OpChatCompletions, Body: []byte(`[1]`)}},
	})
}
