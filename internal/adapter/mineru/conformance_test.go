package mineru

import (
	"fmt"
	"io"
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

	var taskSeq int64
	pollCounts := sync.Map{}

	parseUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, adaptertest.UpstreamPrefix)
		switch {
		case r.Method == http.MethodPost && path == "/v1/tasks":
			id := fmt.Sprintf("task_%d", atomic.AddInt64(&taskSeq, 1))
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":%q,"status":"pending"}`, id)

		case r.Method == http.MethodGet && strings.HasSuffix(path, "/result"):
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = io.WriteString(w, "# Extracted Document\n\nContent from mineru.")

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/tasks/"):
			taskID := strings.TrimPrefix(path, "/v1/tasks/")
			val, _ := pollCounts.LoadOrStore(taskID, new(int32))
			cnt := atomic.AddInt32(val.(*int32), 1)
			w.Header().Set("Content-Type", "application/json")
			if cnt < 2 {
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"running"}`, taskID)
			} else {
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"completed","pages":6}`, taskID)
			}

		default:
			http.NotFound(w, r)
		}
	})

	asyncCreateResp := []byte(`{"id":"task_pin_42","status":"pending"}`)
	asyncGetResp := []byte(`{"id":"task_pin_42","pages":15,"status":"completed"}`)
	zipBytes := []byte("PK\x03\x04samplezipbytes")

	adaptertest.RunEgress(t, eg, adaptertest.EgressFixture{
		Cases: []adaptertest.EgressCase{
			{
				Name:          "sync parse polls until done",
				Envelope:      protocol.Envelope{Op: OpParse, Model: ModelParse},
				NewBodyStream: func() io.Reader { return strings.NewReader("sample document content") },
				Upstream:      parseUpstream,
				WantKind:      protocol.ResponseBuffered,
				WantUsage:     pricing.Usage{UnitPages: 6, UnitRequests: 1},
				WantSource:    protocol.UsageUpstream,
				WantClient:    []byte("# Extracted Document\n\nContent from mineru."),
			},
			{
				Name: "task create returns pins",
				Envelope: protocol.Envelope{
					Op:       OpTaskCreate,
					Model:    ModelParse,
					Estimate: pricing.Usage{UnitPages: 15, UnitRequests: 1},
				},
				NewBodyStream: func() io.Reader { return strings.NewReader("sample upload stream") },
				Upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(asyncCreateResp)
				}),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  pricing.Usage{UnitPages: 15, UnitRequests: 1},
				WantSource: protocol.UsageEstimated,
				WantPins:   []string{"task_pin_42"},
				WantClient: asyncCreateResp,
			},
			{
				Name:     "task get is free",
				Envelope: protocol.Envelope{Op: OpTaskGet, Params: map[string]string{"id": "task_pin_42"}},
				Upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(asyncGetResp)
				}),
				WantKind:   protocol.ResponseBuffered,
				WantUsage:  nil,
				WantSource: protocol.UsageUpstream,
				WantClient: asyncGetResp,
			},
			{
				Name:     "task result downloads stream",
				Envelope: protocol.Envelope{Op: OpTaskResult, Params: map[string]string{"id": "task_pin_42"}},
				Upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/zip")
					w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
					_, _ = w.Write(zipBytes)
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
		},
	})
}
