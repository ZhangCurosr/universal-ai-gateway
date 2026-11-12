package mineru

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// spec/08 §5：每个适配器都必须通过一致性测试套件。
func TestIngressConformance(t *testing.T) {
	zeroUsage := pricing.Usage{UnitPages: 0, UnitRequests: 0}

	adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{
		Valid: []adaptertest.IngressCase{
			{
				Name:         "parse with pages query",
				Op:           OpParse,
				Query:        "pages=12",
				WantModel:    ModelParse,
				WantEstimate: pricing.Usage{UnitPages: 12, UnitRequests: 1},
			},
			{
				Name:         "parse without pages query",
				Op:           OpParse,
				WantModel:    ModelParse,
				WantEstimate: pricing.Usage{UnitRequests: 1},
			},
			{
				Name:         "parse with custom model header",
				Op:           OpParse,
				WantModel:    "custom-mineru",
				WantEstimate: pricing.Usage{UnitRequests: 1},
				Header:       http.Header{"X-Uaig-Model": {"custom-mineru"}},
			},
			{
				Name:         "task.create with max_pages query",
				Op:           OpTaskCreate,
				Query:        "max_pages=20",
				WantModel:    ModelParse,
				WantEstimate: pricing.Usage{UnitPages: 20, UnitRequests: 1},
			},
			{
				Name:         "task.create without query",
				Op:           OpTaskCreate,
				WantModel:    ModelParse,
				WantEstimate: pricing.Usage{UnitRequests: 1},
			},
			{
				Name:         "task.get",
				Op:           OpTaskGet,
				Path:         "/v1/tasks/task_123",
				WantNoModel:  true,
				WantStrict:   true,
				WantEstimate: zeroUsage,
				WantParams:   map[string]string{"id": "task_123"},
			},
			{
				Name:         "task.get with query and model header",
				Op:           OpTaskGet,
				Path:         "/v1/tasks/task_123",
				Query:        "verbose=1",
				Header:       http.Header{"X-Uaig-Model": {"custom-mineru"}},
				WantModel:    "custom-mineru",
				WantStrict:   true,
				WantEstimate: zeroUsage,
				WantParams:   map[string]string{"id": "task_123", "query": "verbose=1"},
			},
			{
				Name:         "task.result",
				Op:           OpTaskResult,
				Path:         "/v1/tasks/task_123/result",
				WantNoModel:  true,
				WantStrict:   true,
				WantEstimate: zeroUsage,
				WantParams:   map[string]string{"id": "task_123"},
			},
		},
		Invalid: []adaptertest.IngressCase{
			{Name: "parse negative pages", Op: OpParse, Query: "pages=-5"},
			{Name: "task.create non-numeric pages", Op: OpTaskCreate, Query: "pages=abc"},
			{Name: "task.create zero pages", Op: OpTaskCreate, Query: "pages=0"},
		},
	})
}

func TestEgressConformance(t *testing.T) {
	eg := Egress{
		InitialPollInterval: 5 * time.Millisecond,
		MaxPollInterval:     20 * time.Millisecond,
	}
	md := "# Extracted Document\n\nContent from mineru."
	zipBytes := resultZip(t, "doc/", md, 0)
	body := []byte(`{"url":"https://example.com/doc.pdf"}`)

	var taskSeq int64
	pollCounts := sync.Map{}
	// v4 上游：提交、查询（第 2 次起完成）、结果包（与 base_url 同主机）。
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, adaptertest.UpstreamPrefix)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && path == "/api/v4/extract/task":
			_, _ = fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"task_id":"task_%d"}}`, atomic.AddInt64(&taskSeq, 1))
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v4/extract/task/"):
			id := strings.TrimPrefix(path, "/api/v4/extract/task/")
			val, _ := pollCounts.LoadOrStore(id, new(int32))
			if atomic.AddInt32(val.(*int32), 1) < 2 {
				_, _ = fmt.Fprintf(w, `{"code":0,"data":{"task_id":%q,"state":"running","extract_progress":{"extracted_pages":2,"total_pages":6}}}`, id)
				return
			}
			zipURL := "http://" + r.Host + adaptertest.UpstreamPrefix + "/files/" + id + ".zip"
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"task_id":%q,"state":"done","full_zip_url":%q}}`, id, zipURL)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/files/"):
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	})
	createResp := []byte(`{"code":0,"msg":"ok","data":{"task_id":"task_pin_42"}}`)
	getResp := []byte(`{"code":0,"msg":"ok","data":{"task_id":"task_pin_42","state":"pending"}}`)

	adaptertest.RunEgress(t, eg, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name:       "sync parse polls until done",
				Envelope:   protocol.Envelope{Op: OpParse, Model: ModelParse, Body: body},
				Upstream:   upstream,
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  pricing.Usage{UnitPages: 6, UnitRequests: 1},
				WantSource: protocol.UsageUpstream,
				WantClient: []byte(md),
			},
			{
				Name: "task create returns pins",
				Envelope: protocol.Envelope{
					Op:       OpTaskCreate,
					Model:    ModelParse,
					Body:     body,
					Estimate: pricing.Usage{UnitPages: 15, UnitRequests: 1},
				},
				Upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(createResp)
				}),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  pricing.Usage{UnitPages: 15, UnitRequests: 1},
				WantSource: protocol.UsageEstimated,
				WantPins:   []string{"task_pin_42"},
				WantClient: createResp,
			},
			{
				Name:     "task get is free",
				Envelope: protocol.Envelope{Op: OpTaskGet, Params: map[string]string{"id": "task_pin_42"}},
				Upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(getResp)
				}),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  nil,
				WantSource: protocol.UsageUpstream,
				WantClient: getResp,
			},
			{
				Name:     "task result downloads zip",
				Envelope: protocol.Envelope{Op: OpTaskResult, Params: map[string]string{"id": "task_9"}},
				Upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/files/task_9.zip") {
						w.Header().Set("Content-Type", "application/zip")
						_, _ = w.Write(zipBytes)
						return
					}
					zipURL := "http://" + r.Host + adaptertest.UpstreamPrefix + "/files/task_9.zip"
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"task_id":"task_9","state":"done","full_zip_url":%q}}`, zipURL)
				}),
				WantKind:   protocol.ResponseRaw,
				WantUsage:  nil,
				WantSource: protocol.UsageUpstream,
				WantClient: zipBytes,
			},
		},
		InvalidRequests: []protocol.Envelope{
			{Op: "unsupported_op"},
			{Op: OpTaskGet, Params: map[string]string{}},
			{Op: OpParse, Body: []byte(`[1]`)},
			{Op: OpParse, Body: []byte(`{"is_ocr":true}`)},
		},
	})
}
