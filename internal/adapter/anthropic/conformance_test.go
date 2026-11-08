package anthropic

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
	t.Skip("TODO(T09A)")
	gen := []byte(`{"model":"suite-model","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{
		Valid: []adaptertest.IngressCase{
			{Name: "generate", Op: OpMessages, Body: gen, WantEstimate: pricing.Usage{
				pricing.UnitInputTokens: pricing.EstimateInputTokens(len(gen)), pricing.UnitOutputTokens: 1024,
			}},
			{Name: "stream", Op: OpMessages, WantStream: true,
				Body: []byte(`{"model":"suite-model","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)},
		},
		Invalid: []adaptertest.IngressCase{
			{Name: "missing model", Op: OpMessages, Body: []byte(`{"max_tokens":10,"messages":[]}`)},
			{Name: "not an object", Op: OpMessages, Body: []byte(`[1,2]`)},
			{Name: "broken json", Op: OpMessages, Body: []byte(`{"model":`)},
		},
	})
}

func TestEgressConformance(t *testing.T) {
	t.Skip("TODO(T09A)")
	streamReq := []byte(`{"model":"alias","max_tokens":10,"stream":true,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	bodyReq := []byte(`{"model":"alias","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	checkBody := func(in []byte) func(*testing.T, *http.Request, []byte) {
		return func(t *testing.T, _ *http.Request, out []byte) {
			if m, _ := jsonx.GetString(out, "model"); m != adaptertest.SuiteUpstreamModel {
				t.Errorf("upstream model = %q", m)
			}
			adaptertest.AssertTopLevelPreserved(t, in, out, "model")
		}
	}
	upstreamBody := []byte(`{"id":"msg_1","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":4}}`)
	adaptertest.RunEgress(t, Egress{}, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name:          "stream",
				Envelope:      protocol.Envelope{Op: OpMessages, Model: "alias", Stream: true, Body: streamReq},
				Upstream:      adaptertest.ServeFile("text/event-stream", "testdata/stream_basic.sse"),
				CheckUpstream: checkBody(streamReq),
				WantKind:      protocol.ResponseEvents,
				WantUsage: pricing.Usage{pricing.UnitInputTokens: 25, pricing.UnitOutputTokens: 15,
					pricing.UnitCacheReadTokens: 2000, pricing.UnitCacheWriteTokens: 100},
				WantSource: protocol.UsageUpstream,
			},
			{
				Name:          "buffered",
				Envelope:      protocol.Envelope{Op: OpMessages, Model: "alias", Body: bodyReq},
				Upstream:      adaptertest.ServeBody("application/json", upstreamBody),
				CheckUpstream: checkBody(bodyReq),
				WantKind:      protocol.ResponseBuffered,
				WantUsage:     pricing.Usage{pricing.UnitInputTokens: 3, pricing.UnitOutputTokens: 4},
				WantSource:    protocol.UsageUpstream,
				WantClient:    upstreamBody,
			},
		},
		InvalidRequests: []protocol.Envelope{{Op: OpMessages, Body: []byte(`[1]`)}},
	})
}
