package tavily

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestEgressUnits(t *testing.T) {
	eg := Egress{}
	units := eg.Units()
	if len(units) != 1 || units[0] != UnitCredits {
		t.Errorf("Units() = %v, want [%q]", units, UnitCredits)
	}
}

func TestEgressPrepare_StripAPIKey(t *testing.T) {
	eg := Egress{}
	clientBody := []byte(`{"api_key":"client-leaked-key-12345","query":"test search","search_depth":"advanced"}`)
	env := protocol.Envelope{
		Op:    OpSearch,
		Model: "tavily-search",
		Body:  clientBody,
	}

	call, err := eg.Prepare(&env, "upstream-tavily")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	// 验证请求体中客户端的 api_key 被彻底删除
	if strings.Contains(string(call.Body), "client-leaked-key-12345") {
		t.Errorf("call.Body contains leaked client api_key: %s", string(call.Body))
	}
	if strings.Contains(string(call.Body), `"api_key"`) {
		t.Errorf("call.Body still has api_key field: %s", string(call.Body))
	}

	// 验证 search_depth=advanced 估计用量为 2 credits
	estCredits, ok := call.State.(int64)
	if !ok || estCredits != 2 {
		t.Errorf("call.State estimated credits = %v, want 2", call.State)
	}

	// 验证其他字段保留
	var parsed map[string]any
	if err := json.Unmarshal(call.Body, &parsed); err != nil {
		t.Fatalf("unmarshaling call.Body: %v", err)
	}
	if parsed["query"] != "test search" {
		t.Errorf("query = %v, want 'test search'", parsed["query"])
	}
	if parsed["search_depth"] != "advanced" {
		t.Errorf("search_depth = %v, want 'advanced'", parsed["search_depth"])
	}

	// 验证原 Envelope.Body 未被修改
	if !bytes.Equal(env.Body, clientBody) {
		t.Errorf("env.Body was modified in place")
	}
}

func TestEgressPrepare_BasicEstimate(t *testing.T) {
	eg := Egress{}
	env := protocol.Envelope{
		Op:    OpSearch,
		Model: "tavily-search",
		Body:  []byte(`{"query":"basic search"}`),
	}
	call, err := eg.Prepare(&env, "upstream-tavily")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	estCredits, ok := call.State.(int64)
	if !ok || estCredits != 1 {
		t.Errorf("call.State estimated credits = %v, want 1", call.State)
	}
}

func TestEgressDo_APIKeyNotSentUpstream(t *testing.T) {
	var (
		receivedBody   []byte
		authHeader     string
		receivedPath   string
		upstreamSecret = "upstream-secret-key-xyz"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		receivedPath = r.URL.Path
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"title":"Test","url":"https://test.com","content":"ok"}]}`))
	}))
	defer srv.Close()

	eg := Egress{UserAgent: "custom-ua"}
	inboundBody := []byte(`{"api_key":"client-secret-999","query":"golang"}`)
	call, err := eg.Prepare(&protocol.Envelope{
		Op:   OpSearch,
		Body: inboundBody,
	}, "tavily-search")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := eg.Do(ctx, protocol.Upstream{
		BaseURL:          srv.URL,
		Secret:           []byte(upstreamSecret),
		Client:           srv.Client(),
		MaxBufferedBytes: 1 << 20,
	}, call)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	// 1. 客户端 api_key 绝对不出现在上游接收到的请求体中
	if strings.Contains(string(receivedBody), "client-secret-999") {
		t.Errorf("client api_key leaked to upstream request body: %s", string(receivedBody))
	}
	if strings.Contains(string(receivedBody), `"api_key"`) {
		t.Errorf("api_key field leaked to upstream: %s", string(receivedBody))
	}

	// 2. 上游凭据通过 Authorization: Bearer 发送
	if authHeader != "Bearer "+upstreamSecret {
		t.Errorf("authHeader = %q, want %q", authHeader, "Bearer "+upstreamSecret)
	}

	// 3. 路径为 /search
	if receivedPath != "/search" {
		t.Errorf("receivedPath = %q, want '/search'", receivedPath)
	}

	// 4. 响应正常
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	u, src := resp.Meter.Usage()
	if src != protocol.UsageEstimated || !u.Equal(pricing.Usage{UnitCredits: 1}) {
		t.Errorf("meter usage = %v, %v; want credits:1, estimated", u, src)
	}
}

func TestEgressDo_UnsupportedOp(t *testing.T) {
	eg := Egress{}
	_, err := eg.Do(context.Background(), protocol.Upstream{}, &protocol.Call{Op: "unsupported"})
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("unsupported op error = %v, want invalid_request", err)
	}
}
