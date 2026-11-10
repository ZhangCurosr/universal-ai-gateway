package rest

import (
	"io"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// spec/08 §5 & §8.3：通用 REST 适配器通过一致性测试套件。
func TestIngressConformance(t *testing.T) {
	adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{
		Valid: []adaptertest.IngressCase{
			{
				Name:         "get call",
				Op:           OpGet,
				Path:         "/rest/suite-model/forecast",
				WantModel:    adaptertest.SuiteModel,
				WantEstimate: pricing.Usage{UnitRequests: 1},
				WantParams:   map[string]string{"method": "GET", "path": "/forecast"},
			},
			{
				Name:         "get with query",
				Op:           OpGet,
				Path:         "/rest/suite-model/search",
				Query:        "q=universal",
				WantModel:    adaptertest.SuiteModel,
				WantEstimate: pricing.Usage{UnitRequests: 1},
				WantParams:   map[string]string{"method": "GET", "path": "/search", "query": "q=universal"},
			},
			{
				Name:         "post call",
				Op:           OpPost,
				Path:         "/rest/suite-model/items",
				WantModel:    adaptertest.SuiteModel,
				WantEstimate: pricing.Usage{UnitRequests: 1},
				WantParams:   map[string]string{"method": "POST", "path": "/items"},
			},
			{
				Name:         "put call",
				Op:           OpPut,
				Path:         "/rest/suite-model/upload",
				WantModel:    adaptertest.SuiteModel,
				WantEstimate: pricing.Usage{UnitRequests: 1},
				WantParams:   map[string]string{"method": "PUT", "path": "/upload"},
			},
			{
				Name:         "delete call",
				Op:           OpDelete,
				Path:         "/rest/suite-model/item/42",
				WantModel:    adaptertest.SuiteModel,
				WantEstimate: pricing.Usage{UnitRequests: 1},
				WantParams:   map[string]string{"method": "DELETE", "path": "/item/42"},
			},
			{
				Name:         "patch call",
				Op:           OpPatch,
				Path:         "/rest/suite-model/user/10",
				WantModel:    adaptertest.SuiteModel,
				WantEstimate: pricing.Usage{UnitRequests: 1},
				WantParams:   map[string]string{"method": "PATCH", "path": "/user/10"},
			},
		},
		Invalid: []adaptertest.IngressCase{
			{Name: "path traversal encoded dot dot", Op: OpGet, Path: "/rest/suite-model/%2e%2e/secret"},
			{Name: "path encoded dot", Op: OpGet, Path: "/rest/suite-model/%2e/secret"},
			{Name: "path traversal mixed case", Op: OpGet, Path: "/rest/suite-model/%2E./secret"},
			{Name: "path with encoded backslash", Op: OpGet, Path: "/rest/suite-model/foo%5cbar"},
			{Name: "path with encoded null", Op: OpGet, Path: "/rest/suite-model/foo%00bar"},
		},
	})
}

func TestEgressConformance(t *testing.T) {
	respBody := []byte(`{"status":"ok","count":42}`)
	postResp := []byte(`{"id":"new_res_123"}`)

	eg := Egress{preserveClassifyHTTP: true}

	adaptertest.RunEgress(t, eg, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name: "get transparent forwarding",
				Envelope: protocol.Envelope{
					Op:     OpGet,
					Model:  adaptertest.SuiteModel,
					Params: map[string]string{"method": "GET", "path": "/api/v1/test"},
				},
				Upstream:   adaptertest.ServeBody("application/json", respBody),
				WantKind:   protocol.ResponseRaw,
				WantUsage:  pricing.Usage{UnitRequests: 1},
				WantSource: protocol.UsageUpstream,
				WantClient: respBody,
			},
			{
				Name: "post stream transparent forwarding",
				Envelope: protocol.Envelope{
					Op:     OpPost,
					Model:  adaptertest.SuiteModel,
					Params: map[string]string{"method": "POST", "path": "/api/v1/create"},
				},
				NewBodyStream: func() io.Reader { return strings.NewReader("sample post stream payload") },
				Upstream:      adaptertest.ServeBody("application/json", postResp),
				WantKind:      protocol.ResponseRaw,
				WantUsage:     pricing.Usage{UnitRequests: 1},
				WantSource:    protocol.UsageUpstream,
				WantClient:    postResp,
			},
		},
		InvalidRequests: []protocol.Envelope{
			{Op: "unsupported_op"},
			{Op: OpGet, Params: map[string]string{"method": "CONNECT"}},
		},
	})
}
