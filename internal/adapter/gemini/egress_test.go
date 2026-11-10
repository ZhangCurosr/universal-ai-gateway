package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// 3. 出站 URL 与头测试（HDR-G-01, HDR-G-02, EG-G-01, EG-G-02）
func TestEgressRequestURLAndHeaders(t *testing.T) {
	tests := []struct {
		name      string
		call      *protocol.Call
		wantPath  string
		wantQuery string
	}{
		{
			name: "generateContent",
			call: &protocol.Call{
				Op:     OpModelsCall,
				Model:  "gemini-2.5-flash",
				Stream: false,
				Params: map[string]string{"method": "generateContent"},
				Body:   []byte(`{}`),
			},
			wantPath:  "/custom-prefix/v1beta/models/gemini-2.5-flash:generateContent",
			wantQuery: "",
		},
		{
			name: "streamGenerateContent",
			call: &protocol.Call{
				Op:     OpModelsCall,
				Model:  "gemini-1.5-pro",
				Stream: true,
				Params: map[string]string{"method": "streamGenerateContent"},
				Body:   []byte(`{}`),
			},
			wantPath:  "/custom-prefix/v1beta/models/gemini-1.5-pro:streamGenerateContent",
			wantQuery: "alt=sse",
		},
		{
			name: "countTokens",
			call: &protocol.Call{
				Op:     OpModelsCall,
				Model:  "gemini-pro",
				Stream: false,
				Params: map[string]string{"method": "countTokens"},
				Body:   []byte(`{}`),
			},
			wantPath:  "/custom-prefix/v1beta/models/gemini-pro:countTokens",
			wantQuery: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedReq *http.Request
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedReq = r
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			inHeader := http.Header{}
			inHeader.Set("Authorization", "Bearer client-token")
			inHeader.Set("x-goog-api-key", "client-token")
			inHeader.Set("x-custom-leak", "leak-me")
			tt.call.Header = inHeader

			eg := Egress{UserAgent: "custom-ua"}
			up := protocol.Upstream{
				BaseURL: srv.URL + "/custom-prefix",
				Secret:  []byte("gemini-upstream-secret-key"),
				Client:  srv.Client(),
			}

			_, err := eg.Do(context.Background(), up, tt.call)
			if err != nil {
				t.Fatalf("Do() error: %v", err)
			}

			if capturedReq.URL.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", capturedReq.URL.Path, tt.wantPath)
			}
			if capturedReq.URL.RawQuery != tt.wantQuery {
				t.Errorf("RawQuery = %q, want %q", capturedReq.URL.RawQuery, tt.wantQuery)
			}
			if capturedReq.Header.Get("x-goog-api-key") != "gemini-upstream-secret-key" {
				t.Errorf("x-goog-api-key = %q, want upstream secret", capturedReq.Header.Get("x-goog-api-key"))
			}
			if capturedReq.Header.Get("User-Agent") != "custom-ua" {
				t.Errorf("User-Agent = %q, want custom-ua", capturedReq.Header.Get("User-Agent"))
			}
			if capturedReq.Header.Get("Authorization") != "" || capturedReq.Header.Get("x-custom-leak") != "" {
				t.Error("client headers were forwarded to upstream")
			}
		})
	}
}

// 4. 用量测试（USE-G-01 ~ USE-G-04）
func TestUsageMetadataAccounting(t *testing.T) {
	// 完整用量字段测试：promptTokenCount, candidatesTokenCount, cachedContentTokenCount, thoughtsTokenCount, toolUsePromptTokenCount
	bodyWithAllFields := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 100,
			"cachedContentTokenCount": 30,
			"toolUsePromptTokenCount": 10,
			"candidatesTokenCount": 50,
			"thoughtsTokenCount": 20
		}
	}`)
	m := &meter{}
	m.observeBody(bodyWithAllFields)
	usage, src := m.Usage()
	if src != protocol.UsageUpstream {
		t.Fatalf("source = %v, want UsageUpstream", src)
	}
	// input_tokens = max(100 - 30, 0) + 10 = 80
	// cache_read_tokens = 30
	// output_tokens = 50 + 20 = 70
	want := pricing.Usage{
		pricing.UnitInputTokens:     80,
		pricing.UnitCacheReadTokens: 30,
		pricing.UnitOutputTokens:    70,
	}
	if !usage.Equal(want) {
		t.Errorf("Usage = %v, want %v", usage, want)
	}

	// 流式累计覆盖测试：后到覆盖先到
	mStream := &meter{}
	mStream.observeEvent(sse.Event{Data: []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`)})
	mStream.observeEvent(sse.Event{Data: []byte(`{"candidates":[{"content":{"parts":[{"text":" world"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)})
	uStream, srcStream := mStream.Usage()
	if srcStream != protocol.UsageUpstream {
		t.Fatalf("stream source = %v", srcStream)
	}
	if uStream.Get(pricing.UnitOutputTokens) != 5 {
		t.Errorf("output_tokens = %d, want 5 (latest overwritten)", uStream.Get(pricing.UnitOutputTokens))
	}

	// 中断流：只有文本没有 usageMetadata，估算输出
	mInterrupted := &meter{}
	mInterrupted.observeEvent(sse.Event{Data: []byte(`{"candidates":[{"content":{"parts":[{"text":"12345678"}]}}]}`)})
	uEst, srcEst := mInterrupted.Usage()
	if srcEst != protocol.UsageEstimated {
		t.Fatalf("source = %v, want UsageEstimated", srcEst)
	}
	// 8 字节文本 -> 8/4 = 2 tokens
	if uEst.Get(pricing.UnitOutputTokens) != 2 {
		t.Errorf("estimated output = %d, want 2", uEst.Get(pricing.UnitOutputTokens))
	}

	// 思考部分文本也计入 textBytes
	mThoughtText := &meter{}
	mThoughtText.observeEvent(sse.Event{Data: []byte(`{"candidates":[{"content":{"parts":[{"text":"thought-text","thought":true},{"text":"answer"}]}}]}`)})
	uThought, _ := mThoughtText.Usage()
	// len("thought-text") = 12, len("answer") = 6, total 18 -> ceil(18/4) = 5
	if uThought.Get(pricing.UnitOutputTokens) != 5 {
		t.Errorf("estimated thought+text output = %d, want 5", uThought.Get(pricing.UnitOutputTokens))
	}

	// countTokens 不计费
	mCount := &meter{isCountTokens: true}
	mCount.observeBody([]byte(`{"totalTokens":10}`))
	uCount, srcCount := mCount.Usage()
	if srcCount != protocol.UsageUpstream || uCount != nil {
		t.Errorf("countTokens Usage() = %v, %v; want nil, UsageUpstream", uCount, srcCount)
	}

	// 空 meter 返回 UsageNone
	mNone := &meter{}
	uNone, srcNone := mNone.Usage()
	if srcNone != protocol.UsageNone || uNone != nil {
		t.Errorf("empty meter Usage() = %v, %v; want nil, UsageNone", uNone, srcNone)
	}
}

// 5. 重试时间提取测试（EG-G-04）
func TestExtractRetryDelay(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantDelay time.Duration
	}{
		{
			name: "standard 30s duration string",
			body: `{
				"error": {
					"code": 429,
					"message": "Resource exhausted",
					"status": "RESOURCE_EXHAUSTED",
					"details": [
						{
							"@type": "type.googleapis.com/google.rpc.RetryInfo",
							"retryDelay": "30s"
						}
					]
				}
			}`,
			wantDelay: 30 * time.Second,
		},
		{
			name: "fractional seconds duration string",
			body: `{
				"error": {
					"code": 429,
					"details": [
						{
							"@type": "google.rpc.RetryInfo",
							"retryDelay": "2.5s"
						}
					]
				}
			}`,
			wantDelay: 2500 * time.Millisecond,
		},
		{
			name: "no retry info in details",
			body: `{
				"error": {
					"code": 429,
					"details": [
						{
							"@type": "type.googleapis.com/google.rpc.ErrorInfo",
							"reason": "RATE_LIMIT"
						}
					]
				}
			}`,
			wantDelay: 0,
		},
		{
			name:      "malformed json",
			body:      `not a json`,
			wantDelay: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRetryDelay([]byte(tt.body))
			if got != tt.wantDelay {
				t.Errorf("extractRetryDelay() = %v, want %v", got, tt.wantDelay)
			}
		})
	}
}

// 6. 流中错误测试（STR-G-01）
func TestStreamFaultDetection(t *testing.T) {
	tests := []struct {
		name     string
		chunk    string
		wantKind protocol.OutcomeKind
	}{
		{
			name:     "rate limited via 429",
			chunk:    `{"error":{"code":429,"message":"Rate limited"}}`,
			wantKind: protocol.OutcomeRateLimited,
		},
		{
			name:     "rate limited via RESOURCE_EXHAUSTED",
			chunk:    `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota"}}`,
			wantKind: protocol.OutcomeRateLimited,
		},
		{
			name:     "overloaded via 503",
			chunk:    `{"error":{"code":503,"message":"Unavailable"}}`,
			wantKind: protocol.OutcomeOverloaded,
		},
		{
			name:     "overloaded via UNAVAILABLE",
			chunk:    `{"error":{"status":"UNAVAILABLE","message":"Backend unavailable"}}`,
			wantKind: protocol.OutcomeOverloaded,
		},
		{
			name:     "other server error",
			chunk:    `{"error":{"code":500,"status":"INTERNAL","message":"Fatal error"}}`,
			wantKind: protocol.OutcomeServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &meter{}
			m.observeEvent(sse.Event{Data: []byte(tt.chunk)})
			fault, ok := m.Fault()
			if !ok {
				t.Fatalf("Fault() ok = false, want true")
			}
			if fault.Kind != tt.wantKind {
				t.Errorf("Fault().Kind = %v, want %v", fault.Kind, tt.wantKind)
			}
		})
	}
}
