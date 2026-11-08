package tavily

import (
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// spec/08 §5：每个适配器都必须通过一致性测试套件。
func TestIngressConformance(t *testing.T) {
	adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{
		Valid: []adaptertest.IngressCase{
			{
				Name:         "search basic",
				Op:           OpSearch,
				WantModel:    ModelSearch,
				WantEstimate: pricing.Usage{UnitCredits: 1},
				Body:         []byte(`{"query":"golang"}`),
			},
			{
				Name:         "search advanced",
				Op:           OpSearch,
				WantModel:    ModelSearch,
				WantEstimate: pricing.Usage{UnitCredits: 2},
				Body:         []byte(`{"query":"golang","search_depth":"advanced"}`),
			},
			{
				Name:         "search with custom model header",
				Op:           OpSearch,
				WantModel:    "custom-tavily-search",
				WantEstimate: pricing.Usage{UnitCredits: 1},
				Header:       http.Header{"X-Uaig-Model": {"custom-tavily-search"}},
				Body:         []byte(`{"query":"golang"}`),
			},
			{
				Name:      "extract",
				Op:        OpExtract,
				WantModel: ModelExtract,
				Body:      []byte(`{"urls":["https://example.com"]}`),
			},
			{
				Name:      "crawl",
				Op:        OpCrawl,
				WantModel: ModelCrawl,
				Body:      []byte(`{"urls":["https://example.com"]}`),
			},
		},
		Invalid: []adaptertest.IngressCase{
			{Name: "not an object", Op: OpSearch, Body: []byte(`[1]`)},
			{Name: "empty body", Op: OpSearch, Body: []byte(``)},
			{Name: "search missing query", Op: OpSearch, Body: []byte(`{"search_depth":"advanced"}`)},
		},
	})
}

func TestEgressConformance(t *testing.T) {
	respWithUsage := []byte(`{"results":[{"title":"Go","url":"https://go.dev","content":"Go programming"}],"usage":{"credits":1}}`)
	respWithoutUsage := []byte(`{"results":[{"title":"Go","url":"https://go.dev","content":"Go programming"}]}`)
	extractResp := []byte(`{"results":[{"url":"https://example.com","raw_content":"Hello world"}],"credits":2}`)
	crawlResp := []byte(`{"results":[]}`)

	adaptertest.RunEgress(t, Egress{}, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name:       "search with usage in response",
				Envelope:   protocol.Envelope{Op: OpSearch, Model: ModelSearch, Body: []byte(`{"query":"golang"}`)},
				Upstream:   adaptertest.ServeBody("application/json", respWithUsage),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  pricing.Usage{UnitCredits: 1},
				WantSource: protocol.UsageUpstream,
				WantClient: respWithUsage,
			},
			{
				Name:       "search without usage in response",
				Envelope:   protocol.Envelope{Op: OpSearch, Model: ModelSearch, Body: []byte(`{"query":"golang"}`)},
				Upstream:   adaptertest.ServeBody("application/json", respWithoutUsage),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  pricing.Usage{UnitCredits: 1},
				WantSource: protocol.UsageEstimated,
				WantClient: respWithoutUsage,
			},
			{
				Name:       "extract with credits in response",
				Envelope:   protocol.Envelope{Op: OpExtract, Model: ModelExtract, Body: []byte(`{"urls":["https://example.com"]}`)},
				Upstream:   adaptertest.ServeBody("application/json", extractResp),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  pricing.Usage{UnitCredits: 2},
				WantSource: protocol.UsageUpstream,
				WantClient: extractResp,
			},
			{
				Name:       "crawl without usage in response",
				Envelope:   protocol.Envelope{Op: OpCrawl, Model: ModelCrawl, Body: []byte(`{"urls":["https://example.com"]}`)},
				Upstream:   adaptertest.ServeBody("application/json", crawlResp),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  pricing.Usage{}, // spec/08 §8.1：没有估计用量，由 reserve 决定
				WantSource: protocol.UsageNone,
				WantClient: crawlResp,
			},
		},
		InvalidRequests: []protocol.Envelope{
			{Op: OpSearch, Body: []byte(`[1]`)},
			{Op: OpSearch, Body: []byte(``)},
		},
	})
}
