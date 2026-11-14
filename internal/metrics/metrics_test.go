package metrics_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/metrics"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func setupGoldenCollectorAndGauges() (*metrics.Collector, metrics.Gauges) {
	c := metrics.NewCollector()
	c.ObserveRequest(store.AuditRecord{
		RequestID:        "req-1",
		ClientID:         100,
		Model:            "claude-3-5-sonnet",
		IngressProtocol:  "anthropic",
		UpstreamProtocol: "anthropic",
		Status:           200,
		Outcome:          "success",
		Attempts:         1,
		Stream:           false,
		LatencyMillis:    250,
		TTFBMillis:       100,
	})
	c.ObserveRequest(store.AuditRecord{
		RequestID:        "req-2",
		ClientID:         100,
		Model:            "claude-3-5-sonnet",
		IngressProtocol:  "anthropic",
		UpstreamProtocol: "anthropic",
		Status:           500,
		ErrorCode:        "upstream_error",
		Outcome:          "upstream_error",
		Attempts:         2,
		Stream:           true,
		LatencyMillis:    1500,
		TTFBMillis:       -1, // no first byte
	})
	c.ObserveRequest(store.AuditRecord{
		RequestID:        "req-3",
		ClientID:         200,
		Model:            "gpt-4o",
		IngressProtocol:  "openai_chat",
		UpstreamProtocol: "openai_chat",
		Status:           200,
		Outcome:          "success",
		Attempts:         1,
		Stream:           true,
		LatencyMillis:    800,
		TTFBMillis:       200,
	})

	g := metrics.Gauges{
		Keys: func() []metrics.KeyGauge {
			return []metrics.KeyGauge{
				{
					Pool:              "anthropic",
					ID:                1,
					Name:              "key-a",
					Persisted:         "ready",
					InFlight:          2,
					CooldownRemaining: 0,
					BreakerOpen:       false,
				},
				{
					Pool:              "openai",
					ID:                2,
					Name:              "key-b",
					Persisted:         "quarantined",
					InFlight:          0,
					CooldownRemaining: 5 * time.Second,
					BreakerOpen:       true,
				},
			}
		},
		LedgerQueue: func() (length, capacity int) {
			return 5, 100
		},
		InFlight: func() (requests, bodyBytes int64) {
			return 3, 1024
		},
		AuditDropped: func() int64 {
			return 4
		},
		Version:   "1.0.0",
		StartedAt: time.Unix(1700000000, 0).UTC(),
		Goroutines: func() int {
			return 42
		},
	}

	return c, g
}

func TestFormatGolden(t *testing.T) {
	c, g := setupGoldenCollectorAndGauges()

	var buf bytes.Buffer
	if err := c.WriteMetrics(&buf, g); err != nil {
		t.Fatalf("WriteMetrics failed: %v", err)
	}

	goldenPath := filepath.Join("testdata", "expected.txt")
	expected, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file %s failed: %v", goldenPath, err)
	}

	// Normalise CRLF if running on Windows to compare pure LF
	gotStr := strings.ReplaceAll(buf.String(), "\r\n", "\n")
	wantStr := strings.ReplaceAll(string(expected), "\r\n", "\n")

	if gotStr != wantStr {
		t.Fatalf("golden mismatch:\n--- GOT ---\n%s\n--- WANT ---\n%s", gotStr, wantStr)
	}
}

func TestLabelEscaping(t *testing.T) {
	c := metrics.NewCollector()
	c.ObserveRequest(store.AuditRecord{
		Model:            `model"quote\backslash` + "\nnewline",
		IngressProtocol:  `in"gress`,
		UpstreamProtocol: `up\stream`,
		Status:           400,
		ErrorCode:        `err"code\`,
		Outcome:          `bad\outcome`,
		Attempts:         1,
		LatencyMillis:    100,
		TTFBMillis:       50,
	})

	var buf bytes.Buffer
	if err := c.WriteMetrics(&buf, metrics.Gauges{}); err != nil {
		t.Fatalf("WriteMetrics failed: %v", err)
	}
	out := buf.String()

	// Assert escaping
	if !strings.Contains(out, `model="model\"quote\\backslash\nnewline"`) {
		t.Errorf("escaped model not found in output: %s", out)
	}
	if !strings.Contains(out, `in\"gress`) {
		t.Errorf("escaped ingress not found in output: %s", out)
	}
	if !strings.Contains(out, `up\\stream`) {
		t.Errorf("escaped upstream not found in output: %s", out)
	}
	if !strings.Contains(out, `err\"code\\`) {
		t.Errorf("escaped error_code not found in output: %s", out)
	}
}

func TestHistogramRules(t *testing.T) {
	c := metrics.NewCollector()

	// 1. Observe multiple requests with different latencies and TTFB
	c.ObserveRequest(store.AuditRecord{
		Model:         "m1",
		LatencyMillis: 50, // 0.05s <= 0.1s
		TTFBMillis:    -1, // no first byte!
	})
	c.ObserveRequest(store.AuditRecord{
		Model:         "m1",
		LatencyMillis: 200, // 0.20s <= 0.25s
		TTFBMillis:    60,  // 0.06s <= 0.10s
	})
	c.ObserveRequest(store.AuditRecord{
		Model:         "m1",
		LatencyMillis: 800, // 0.80s <= 1.0s
		TTFBMillis:    150, // 0.15s <= 0.25s
	})

	var buf bytes.Buffer
	if err := c.WriteMetrics(&buf, metrics.Gauges{}); err != nil {
		t.Fatalf("WriteMetrics failed: %v", err)
	}
	out := buf.String()

	// TTFBMillis = -1 should NOT be counted in ttfb histogram (count should be 2, not 3)
	if !strings.Contains(out, `uaig_ttfb_seconds_count{model="m1",stream="false"} 2`) {
		t.Errorf("expected ttfb count 2, output was:\n%s", out)
	}
	// Duration count should be 3
	if !strings.Contains(out, `uaig_request_duration_seconds_count{model="m1",stream="false"} 3`) {
		t.Errorf("expected duration count 3, output was:\n%s", out)
	}
	// +Inf bucket equals _count
	if !strings.Contains(out, `uaig_request_duration_seconds_bucket{model="m1",stream="false",le="+Inf"} 3`) {
		t.Errorf("expected duration +Inf bucket to equal count 3")
	}
	if !strings.Contains(out, `uaig_ttfb_seconds_bucket{model="m1",stream="false",le="+Inf"} 2`) {
		t.Errorf("expected ttfb +Inf bucket to equal count 2")
	}
	// Cumulative check:
	// For duration (0.05, 0.20, 0.80):
	// le="0.1" -> 1 (0.05)
	// le="0.25" -> 2 (0.05, 0.20)
	// le="1" -> 3 (0.05, 0.20, 0.80)
	if !strings.Contains(out, `uaig_request_duration_seconds_bucket{model="m1",stream="false",le="0.1"} 1`) {
		t.Errorf("expected duration le=0.1 to be 1")
	}
	if !strings.Contains(out, `uaig_request_duration_seconds_bucket{model="m1",stream="false",le="0.25"} 2`) {
		t.Errorf("expected duration le=0.25 to be 2")
	}
	if !strings.Contains(out, `uaig_request_duration_seconds_bucket{model="m1",stream="false",le="1"} 3`) {
		t.Errorf("expected duration le=1 to be 3")
	}
	// Sum: 0.05 + 0.2 + 0.8 = 1.05
	if !strings.Contains(out, `uaig_request_duration_seconds_sum{model="m1",stream="false"} 1.05`) {
		t.Errorf("expected duration sum 1.05")
	}
}

func TestCardinalityAndPrivacy(t *testing.T) {
	c := metrics.NewCollector()
	c.ObserveRequest(store.AuditRecord{
		RequestID:        "req-super-secret-xyz-777",
		ClientID:         88888,
		Model:            "model-safe",
		IngressProtocol:  "anthropic",
		UpstreamProtocol: "anthropic",
		Status:           200,
		Outcome:          "success",
		Attempts:         1,
		LatencyMillis:    100,
		TTFBMillis:       50,
	})

	var buf bytes.Buffer
	if err := c.WriteMetrics(&buf, metrics.Gauges{}); err != nil {
		t.Fatalf("WriteMetrics failed: %v", err)
	}
	out := buf.String()

	// ClientID must not appear
	if strings.Contains(out, "88888") {
		t.Errorf("forbidden client ID leaked in metrics output")
	}
	// RequestID must not appear
	if strings.Contains(out, "req-super-secret-xyz-777") {
		t.Errorf("forbidden request ID leaked in metrics output")
	}
}

func TestConcurrency(t *testing.T) {
	c := metrics.NewCollector()

	const goroutines = 50
	const perG = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(gid int) {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				c.ObserveRequest(store.AuditRecord{
					Model:            "concurrent-model",
					IngressProtocol:  "openai_chat",
					UpstreamProtocol: "openai_chat",
					Status:           200,
					Outcome:          "success",
					Attempts:         1,
					LatencyMillis:    100,
					TTFBMillis:       50,
				})
			}
		}(i)
	}

	// Concurrently read while observing
	doneScrape := make(chan struct{})
	go func() {
		for {
			select {
			case <-doneScrape:
				return
			default:
				var buf bytes.Buffer
				_ = c.WriteMetrics(&buf, metrics.Gauges{})
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	wg.Wait()
	close(doneScrape)

	var finalBuf bytes.Buffer
	if err := c.WriteMetrics(&finalBuf, metrics.Gauges{}); err != nil {
		t.Fatalf("final WriteMetrics failed: %v", err)
	}
	out := finalBuf.String()

	expectedCount := goroutines * perG
	want := fmt.Sprintf(`uaig_requests_total{code="2xx",ingress="openai_chat",model="concurrent-model",outcome="success",upstream="openai_chat"} %d`, expectedCount)
	if !strings.Contains(out, want) {
		t.Fatalf("expected exact requests count %d, got output:\n%s", expectedCount, out)
	}
}

func TestGaugesNilFieldsOmitted(t *testing.T) {
	c := metrics.NewCollector()
	var buf bytes.Buffer
	// All gauges nil
	if err := c.WriteMetrics(&buf, metrics.Gauges{}); err != nil {
		t.Fatalf("WriteMetrics failed: %v", err)
	}
	out := buf.String()

	// Only uaig_goroutines should be present (defaults to runtime.NumGoroutine)
	if !strings.Contains(out, "uaig_goroutines") {
		t.Errorf("expected uaig_goroutines to be present by default")
	}
	if strings.Contains(out, "uaig_audit_dropped_total") {
		t.Errorf("expected uaig_audit_dropped_total to be omitted when nil")
	}
	if strings.Contains(out, "uaig_build_info") {
		t.Errorf("expected uaig_build_info to be omitted when empty")
	}
	if strings.Contains(out, "uaig_key_inflight") {
		t.Errorf("expected uaig_key_inflight to be omitted when nil")
	}
	if strings.Contains(out, "uaig_ledger_events_queue_length") {
		t.Errorf("expected uaig_ledger_events_queue_length to be omitted when nil")
	}
	if strings.Contains(out, "uaig_overload_inflight_requests") {
		t.Errorf("expected uaig_overload_inflight_requests to be omitted when nil")
	}
	if strings.Contains(out, "uaig_start_time_seconds") {
		t.Errorf("expected uaig_start_time_seconds to be omitted when zero")
	}
}

func TestHandler(t *testing.T) {
	c, g := setupGoldenCollectorAndGauges()
	handler := c.Handler(g)

	// GET -> 200 and Content-Type text/plain; version=0.0.4; charset=utf-8
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/metrics status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != metrics.ContentType {
		t.Fatalf("Content-Type = %s, want %s", ct, metrics.ContentType)
	}
	if !strings.Contains(rec.Body.String(), "uaig_build_info") {
		t.Fatalf("expected uaig_build_info in body")
	}

	// POST -> 405 Method Not Allowed with Allow: GET
	recPost := httptest.NewRecorder()
	reqPost := httptest.NewRequest(http.MethodPost, "/admin/metrics", nil)
	handler.ServeHTTP(recPost, reqPost)

	if recPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /admin/metrics status = %d, want 405", recPost.Code)
	}
	allow := recPost.Header().Get("Allow")
	if allow != http.MethodGet {
		t.Fatalf("Allow header = %s, want GET", allow)
	}
}

// MTR-05：RestrictModels 之后，不在已配置清单里的模型名（客户端可任意填写）一律记为 "other"，
// 大量不同的未知模型名只产生一组时间序列。
func TestRestrictModelsBoundsCardinality(t *testing.T) {
	c := metrics.NewCollector()
	c.RestrictModels([]string{"claude-known"})
	c.ObserveRequest(store.AuditRecord{Model: "claude-known", Status: 200, LatencyMillis: 10, TTFBMillis: -1})
	for i := 0; i < 50; i++ {
		c.ObserveRequest(store.AuditRecord{Model: fmt.Sprintf("attacker-chosen-%d", i), Status: 404,
			ErrorCode: "model_not_found", LatencyMillis: 1, TTFBMillis: -1})
	}
	c.ObserveRequest(store.AuditRecord{Model: "", Status: 400, ErrorCode: "invalid_request", LatencyMillis: 1, TTFBMillis: -1})

	var buf bytes.Buffer
	if err := c.WriteMetrics(&buf, metrics.Gauges{}); err != nil {
		t.Fatalf("WriteMetrics failed: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "attacker-chosen") {
		t.Fatalf("unconfigured model name leaked into labels:\n%s", out)
	}
	if strings.Contains(out, `model=""`) {
		t.Errorf("empty model label should be reported as other:\n%s", out)
	}
	if !strings.Contains(out, `model="claude-known"`) {
		t.Errorf("configured model missing:\n%s", out)
	}
	want := `uaig_request_errors_total{error_code="model_not_found",model="other"} 50`
	if !strings.Contains(out, want) {
		t.Errorf("want line %q in output:\n%s", want, out)
	}
}

// MTR-13：池级容量与告警指标；没有额度或过期窗口的池不输出对应指标
func TestPoolGauges(t *testing.T) {
	c := metrics.NewCollector()
	var buf strings.Builder
	err := c.WriteMetrics(&buf, metrics.Gauges{Pools: func() []metrics.PoolGauge {
		return []metrics.PoolGauge{
			{Pool: "tv", Available: 3, HasQuota: true, QuotaLeft: 120, HasExpiry: true, Expiring: 2, FiringAlerts: []string{"expiring", "low_quota"}},
			{Pool: `we"ird`, Available: 0},
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`uaig_pool_available_keys{pool="tv"} 3`,
		`uaig_pool_available_keys{pool="we\"ird"} 0`,
		`uaig_pool_quota_left{pool="tv"} 120`,
		`uaig_pool_keys_expiring{pool="tv"} 2`,
		`uaig_alerts_firing{kind="expiring",pool="tv"} 1`,
		`uaig_alerts_firing{kind="low_quota",pool="tv"} 1`,
		"# TYPE uaig_pool_available_keys gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `uaig_pool_quota_left{pool="we`) || strings.Contains(out, `uaig_pool_keys_expiring{pool="we`) {
		t.Error("pools without quota or expiry window must not export those gauges")
	}
}
