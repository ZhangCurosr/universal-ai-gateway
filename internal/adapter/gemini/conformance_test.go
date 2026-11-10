package gemini

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// spec/08 §5 & §10：Gemini 适配器通过一致性测试套件。
func TestIngressConformance(t *testing.T) {
	gen := []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":512}}`)
	adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{
		Valid: []adaptertest.IngressCase{
			{
				Name: "generate",
				Op:   OpModelsCall,
				Path: "/v1beta/models/suite-model:generateContent",
				Body: gen,
				WantEstimate: pricing.Usage{
					pricing.UnitInputTokens:  pricing.EstimateInputTokens(len(gen)),
					pricing.UnitOutputTokens: 512,
				},
				WantParams: map[string]string{"method": "generateContent"},
			},
			{
				Name:       "stream",
				Op:         OpModelsCall,
				Path:       "/v1beta/models/suite-model:streamGenerateContent",
				Query:      "alt=sse",
				WantStream: true,
				Body:       []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`),
				WantParams: map[string]string{"method": "streamGenerateContent"},
			},
			{
				Name: "countTokens",
				Op:   OpModelsCall,
				Path: "/v1beta/models/suite-model:countTokens",
				Body: []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`),
				WantEstimate: pricing.Usage{
					pricing.UnitInputTokens:     0,
					pricing.UnitOutputTokens:    0,
					pricing.UnitCacheReadTokens: 0,
				},
				WantParams: map[string]string{"method": "countTokens"},
			},
		},
		Invalid: []adaptertest.IngressCase{
			{Name: "missing colon", Op: OpModelsCall, Path: "/v1beta/models/suite-model", Body: []byte(`{}`)},
			{Name: "unsupported method", Op: OpModelsCall, Path: "/v1beta/models/suite-model:embedContent", Body: []byte(`{}`)},
			{Name: "stream without alt=sse", Op: OpModelsCall, Path: "/v1beta/models/suite-model:streamGenerateContent", Body: []byte(`{}`)},
			{Name: "not an object", Op: OpModelsCall, Path: "/v1beta/models/suite-model:generateContent", Body: []byte(`[1,2]`)},
			{Name: "broken json", Op: OpModelsCall, Path: "/v1beta/models/suite-model:generateContent", Body: []byte(`{"contents":`)},
			{Name: "maxOutputTokens zero", Op: OpModelsCall, Path: "/v1beta/models/suite-model:generateContent", Body: []byte(`{"generationConfig":{"maxOutputTokens":0}}`)},
			{Name: "maxOutputTokens string", Op: OpModelsCall, Path: "/v1beta/models/suite-model:generateContent", Body: []byte(`{"generationConfig":{"maxOutputTokens":"512"}}`)},
		},
	})
}

func TestEgressConformance(t *testing.T) {
	streamReq := []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
	bodyReq := []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
	upstreamBody := []byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,"cachedContentTokenCount":2,"thoughtsTokenCount":1,"toolUsePromptTokenCount":3}}`)
	countTokensReq := []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
	countTokensBody := []byte(`{"totalTokens":10}`)

	checkBodyPreserved := func(in []byte) func(*testing.T, *http.Request, []byte) {
		return func(t *testing.T, _ *http.Request, out []byte) {
			if !bytes.Equal(bytes.TrimSpace(in), bytes.TrimSpace(out)) {
				t.Errorf("upstream body changed:\ngot  %s\nwant %s", out, in)
			}
		}
	}

	adaptertest.RunEgress(t, Egress{}, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name:          "stream",
				Envelope:      protocol.Envelope{Op: OpModelsCall, Model: "alias", Stream: true, Params: map[string]string{"method": "streamGenerateContent"}, Body: streamReq},
				Upstream:      adaptertest.ServeFile("text/event-stream", "testdata/stream_basic.sse"),
				CheckUpstream: checkBodyPreserved(streamReq),
				WantKind:      protocol.ResponseEvents,
				WantUsage:     pricing.Usage{pricing.UnitInputTokens: 10, pricing.UnitOutputTokens: 6, pricing.UnitCacheReadTokens: 5},
				WantSource:    protocol.UsageUpstream,
			},
			{
				Name:          "buffered",
				Envelope:      protocol.Envelope{Op: OpModelsCall, Model: "alias", Params: map[string]string{"method": "generateContent"}, Body: bodyReq},
				Upstream:      adaptertest.ServeBody("application/json", upstreamBody),
				CheckUpstream: checkBodyPreserved(bodyReq),
				WantKind:      protocol.ResponseBuffered,
				WantUsage:     pricing.Usage{pricing.UnitInputTokens: 11, pricing.UnitOutputTokens: 5, pricing.UnitCacheReadTokens: 2},
				WantSource:    protocol.UsageUpstream,
				WantClient:    upstreamBody,
			},
			{
				Name:          "countTokens",
				Envelope:      protocol.Envelope{Op: OpModelsCall, Model: "alias", Params: map[string]string{"method": "countTokens"}, Body: countTokensReq},
				Upstream:      adaptertest.ServeBody("application/json", countTokensBody),
				CheckUpstream: checkBodyPreserved(countTokensReq),
				WantKind:      protocol.ResponseBuffered,
				WantUsage:     nil,
				WantSource:    protocol.UsageUpstream,
				WantClient:    countTokensBody,
			},
		},
		InvalidRequests: []protocol.Envelope{{Op: OpModelsCall, Body: []byte(`[1]`)}},
	})
}
