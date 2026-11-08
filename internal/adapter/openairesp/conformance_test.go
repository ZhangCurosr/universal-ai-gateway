package openairesp

import (
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

var zeroUsage = pricing.Usage{pricing.UnitInputTokens: 0, pricing.UnitOutputTokens: 0, pricing.UnitCacheReadTokens: 0}

// spec/08 §5：每个适配器都必须通过一致性测试套件。
func TestIngressConformance(t *testing.T) {
	t.Skip("TODO(T09C)")
	gen := []byte(`{"model":"suite-model","input":"hi","max_output_tokens":256}`)
	adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{
		Valid: []adaptertest.IngressCase{
			{Name: "create", Op: OpCreate, Body: gen, WantEstimate: pricing.Usage{
				pricing.UnitInputTokens: pricing.EstimateInputTokens(len(gen)), pricing.UnitOutputTokens: 256,
			}},
			{Name: "create stream", Op: OpCreate, WantStream: true, Body: []byte(`{"model":"suite-model","input":"hi","stream":true}`)},
			{Name: "create continues previous", Op: OpCreate, WantStrict: true,
				Body: []byte(`{"model":"suite-model","input":"more","previous_response_id":"resp_1"}`)},
			{Name: "get without model", Op: OpGet, Path: "/v1/responses/resp_1", WantNoModel: true, WantStrict: true,
				WantEstimate: zeroUsage, WantParams: map[string]string{"id": "resp_1"}},
			{Name: "get with model header", Op: OpGet, Path: "/v1/responses/resp_1", Query: "include[]=x",
				Header: http.Header{"X-Uaig-Model": {"suite-model"}}, WantStrict: true,
				WantParams: map[string]string{"id": "resp_1", "query": "include[]=x"}},
			{Name: "delete", Op: OpDelete, Path: "/v1/responses/resp_1", WantNoModel: true, WantStrict: true},
			{Name: "cancel", Op: OpCancel, Path: "/v1/responses/resp_1/cancel", WantNoModel: true, WantStrict: true},
			{Name: "input items", Op: OpInputItems, Path: "/v1/responses/resp_1/input_items", WantNoModel: true, WantStrict: true},
		},
		Invalid: []adaptertest.IngressCase{
			{Name: "missing model", Op: OpCreate, Body: []byte(`{"input":"hi"}`)},
			{Name: "not an object", Op: OpCreate, Body: []byte(`[1]`)},
			{Name: "bad max_output_tokens", Op: OpCreate, Body: []byte(`{"model":"m","input":"hi","max_output_tokens":"x"}`)},
		},
	})
}

func TestEgressConformance(t *testing.T) {
	t.Skip("TODO(T09C)")
	streamReq := []byte(`{"model":"alias","input":"hi","stream":true,"tools":[{"type":"web_search"}]}`)
	bodyReq := []byte(`{"model":"alias","input":"hi","store":false}`)
	checkBody := func(in []byte) func(*testing.T, *http.Request, []byte) {
		return func(t *testing.T, _ *http.Request, out []byte) {
			if m, _ := jsonx.GetString(out, "model"); m != adaptertest.SuiteUpstreamModel {
				t.Errorf("upstream model = %q", m)
			}
			adaptertest.AssertTopLevelPreserved(t, in, out, "model")
		}
	}
	stored := []byte(`{"id":"resp_9","object":"response","status":"completed","output":[],"usage":{"input_tokens":9,"input_tokens_details":{"cached_tokens":0},"output_tokens":2}}`)
	unstored := []byte(`{"id":"resp_8","object":"response","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":1}}`)
	adaptertest.RunEgress(t, Egress{}, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name:          "stream pins response id",
				Envelope:      protocol.Envelope{Op: OpCreate, Model: "alias", Stream: true, Body: streamReq},
				Upstream:      adaptertest.ServeFile("text/event-stream", "testdata/stream_basic.sse"),
				CheckUpstream: checkBody(streamReq),
				WantKind:      protocol.ResponseEvents,
				WantUsage:     pricing.Usage{pricing.UnitInputTokens: 20, pricing.UnitOutputTokens: 7, pricing.UnitCacheReadTokens: 100},
				WantSource:    protocol.UsageUpstream,
				WantPins:      []string{"resp_abc123"},
			},
			{
				Name:          "buffered store=false is not pinned",
				Envelope:      protocol.Envelope{Op: OpCreate, Model: "alias", Body: bodyReq},
				Upstream:      adaptertest.ServeBody("application/json", unstored),
				CheckUpstream: checkBody(bodyReq),
				WantKind:      protocol.ResponseBuffered,
				WantUsage:     pricing.Usage{pricing.UnitInputTokens: 5, pricing.UnitOutputTokens: 1},
				WantSource:    protocol.UsageUpstream,
				WantClient:    unstored,
			},
			{
				Name:     "get is free",
				Envelope: protocol.Envelope{Op: OpGet, Params: map[string]string{"id": "resp_9"}},
				Upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/v1/responses/resp_9" {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(stored)
				}),
				WantKind:   protocol.ResponseBuffered,
				WantSource: protocol.UsageUpstream,
				WantClient: stored,
			},
		},
		InvalidRequests: []protocol.Envelope{{Op: OpCreate, Body: []byte(`[1]`)}},
	})
}
