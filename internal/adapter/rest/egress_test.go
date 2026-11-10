package rest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestEgress_HeaderFilteringAndForwarding(t *testing.T) {
	var capturedHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", "\"12345\"")
		w.Header().Set("Content-Disposition", "inline; filename=\"test.json\"")
		w.Header().Set("Set-Cookie", "session=upstream-secret")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	eg := Egress{}
	clientHeader := http.Header{
		"Content-Type":                      {"application/json"},
		"Accept":                            {"application/json"},
		"Accept-Language":                   {"zh-CN,en;q=0.9"},
		"If-None-Match":                     {"\"etag-1\""},
		"Range":                             {"bytes=0-100"},
		"Authorization":                     {"Bearer client-token-must-drop"},
		"Cookie":                            {"client-session=must-drop"},
		"X-Api-Key":                         {"client-api-key-must-drop"},
		"X-Forwarded-For":                   {"203.0.113.1"},
		"X-UAIG-Forward-X-Api-Version":      {"2024-05-01"},
		"X-UAIG-Forward-X-Tenant-Id":        {"tenant-42"},
		"X-UAIG-Forward-Authorization":      {"Bearer should-be-dropped"},
		"X-UAIG-Forward-Host":               {"evil.com"},
		"X-UAIG-Forward-X-Forwarded-For":    {"10.0.0.1"},
		"X-UAIG-Forward-Cookie":             {"auth=dropped"},
		"X-UAIG-Forward-X-Api-Key":          {"dropped-key"},
		"X-UAIG-Forward-X-Goog-Api-Key":     {"dropped-goog-key"},
		"X-UAIG-Forward-Proxy-Authorization": {"dropped-proxy"},
	}

	call := &protocol.Call{
		Op:     OpGet,
		Model:  "test-svc",
		Header: clientHeader,
		Params: map[string]string{
			"method": "GET",
			"path":   "/api/v1/resource",
		},
	}

	up := protocol.Upstream{
		BaseURL: srv.URL,
		Secret:  []byte("bearer:upstream-secret-token"),
		Client:  srv.Client(),
	}

	resp, err := eg.Do(context.Background(), up, call)
	if err != nil {
		t.Fatalf("unexpected Do error: %v", err)
	}
	defer resp.Raw.Close()

	// 验证白名单头已转发到上游
	if capturedHeader.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type not forwarded")
	}
	if capturedHeader.Get("Accept") != "application/json" {
		t.Errorf("Accept not forwarded")
	}
	if capturedHeader.Get("Accept-Language") != "zh-CN,en;q=0.9" {
		t.Errorf("Accept-Language not forwarded")
	}
	if capturedHeader.Get("If-None-Match") != "\"etag-1\"" {
		t.Errorf("If-None-Match not forwarded")
	}
	if capturedHeader.Get("Range") != "bytes=0-100" {
		t.Errorf("Range not forwarded")
	}

	// 验证客户端敏感头绝对没有直接转发
	if capturedHeader.Get("Cookie") != "" {
		t.Errorf("client Cookie was forwarded upstream: %v", capturedHeader.Get("Cookie"))
	}
	if capturedHeader.Get("X-Forwarded-For") != "" {
		t.Errorf("client X-Forwarded-For was forwarded upstream: %v", capturedHeader.Get("X-Forwarded-For"))
	}

	// 验证上游凭据头正确注入
	if capturedHeader.Get("Authorization") != "Bearer upstream-secret-token" {
		t.Errorf("Authorization = %q, want 'Bearer upstream-secret-token'", capturedHeader.Get("Authorization"))
	}

	// 验证 X-UAIG-Forward-<Name> 机制
	if capturedHeader.Get("X-Api-Version") != "2024-05-01" {
		t.Errorf("X-Api-Version = %q, want '2024-05-01'", capturedHeader.Get("X-Api-Version"))
	}
	if capturedHeader.Get("X-Tenant-Id") != "tenant-42" {
		t.Errorf("X-Tenant-Id = %q, want 'tenant-42'", capturedHeader.Get("X-Tenant-Id"))
	}

	// 验证被禁止的 forward 头被丢弃
	if capturedHeader.Get("Host") == "evil.com" {
		t.Errorf("X-UAIG-Forward-Host was forwarded")
	}

	// 验证响应头
	if resp.Header.Get("ETag") != "\"12345\"" {
		t.Errorf("ETag = %q, want '\"12345\"'", resp.Header.Get("ETag"))
	}
	if resp.Header.Get("Content-Disposition") != "inline; filename=\"test.json\"" {
		t.Errorf("Content-Disposition not forwarded")
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("upstream Set-Cookie forwarded to client (FWD-04 violation)")
	}

	// 验证 Meter
	u, src := resp.Meter.Usage()
	if src != protocol.UsageUpstream || u[UnitRequests] != 1 {
		t.Errorf("Meter = %v, %v; want requests: 1, UsageUpstream", u, src)
	}
}

func TestEgress_KeyOverwritesAndQueryStrip(t *testing.T) {
	var capturedHeader http.Header
	var capturedURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		capturedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	eg := Egress{}

	// 测试 1: header 方式覆盖同名 forward 头
	t.Run("header overwrites client forward header", func(t *testing.T) {
		call := &protocol.Call{
			Op: OpGet,
			Header: http.Header{
				"X-UAIG-Forward-X-Api-Key": {"client-fake-key"}, // 虽然被丢弃，但测试即使有也必须被 Key 覆盖
				"X-UAIG-Forward-X-Custom":  {"client-custom-val"},
			},
			Params: map[string]string{
				"method": "GET",
				"path":   "/test",
			},
		}
		up := protocol.Upstream{
			BaseURL: srv.URL,
			Secret:  []byte("header:X-Custom:upstream-injected-val"),
			Client:  srv.Client(),
		}
		resp, err := eg.Do(context.Background(), up, call)
		if err != nil {
			t.Fatalf("Do error: %v", err)
		}
		defer resp.Raw.Close()

		if capturedHeader.Get("X-Custom") != "upstream-injected-val" {
			t.Errorf("X-Custom = %q, want 'upstream-injected-val'", capturedHeader.Get("X-Custom"))
		}
	})

	// 测试 2: query 方式剔除客户端同名参数并注入新参数
	t.Run("query key strips client same-name param", func(t *testing.T) {
		call := &protocol.Call{
			Op: OpGet,
			Params: map[string]string{
				"method": "GET",
				"path":   "/search",
				"query":  "city=Paris&token=client-old-token&page=2&token=duplicate-client-token",
			},
		}
		up := protocol.Upstream{
			BaseURL: srv.URL,
			Secret:  []byte("query:token:upstream-secret-token"),
			Client:  srv.Client(),
		}
		resp, err := eg.Do(context.Background(), up, call)
		if err != nil {
			t.Fatalf("Do error: %v", err)
		}
		defer resp.Raw.Close()

		if strings.Contains(capturedURL, "client-old-token") || strings.Contains(capturedURL, "duplicate-client-token") {
			t.Errorf("client token was not stripped: URL = %s", capturedURL)
		}
		if !strings.Contains(capturedURL, "token=upstream-secret-token") {
			t.Errorf("upstream token not injected: URL = %s", capturedURL)
		}
		if !strings.Contains(capturedURL, "city=Paris") || !strings.Contains(capturedURL, "page=2") {
			t.Errorf("other query params were lost: URL = %s", capturedURL)
		}
	})
}

func TestEgress_Status403Rewrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"access_denied_to_resource"}`))
	}))
	defer srv.Close()

	eg := Egress{} // 生产环境配置（preserveClassifyHTTP = false）
	call := &protocol.Call{
		Op:     OpGet,
		Params: map[string]string{"method": "GET", "path": "/forbidden"},
	}
	up := protocol.Upstream{
		BaseURL: srv.URL,
		Secret:  []byte("bearer:valid-key"),
		Client:  srv.Client(),
	}

	_, err := eg.Do(context.Background(), up, call)
	if err == nil {
		t.Fatalf("expected error on 403, got nil")
	}

	var attErr *protocol.AttemptError
	if !errors.As(err, &attErr) {
		t.Fatalf("expected *protocol.AttemptError, got %T: %v", err, err)
	}

	// RS-16: 403 改写为 OutcomeClientError，保留 403 状态码与响应体
	if attErr.Outcome.Kind != protocol.OutcomeClientError {
		t.Errorf("Outcome.Kind = %v, want OutcomeClientError", attErr.Outcome.Kind)
	}
	if attErr.Outcome.Status != http.StatusForbidden {
		t.Errorf("Outcome.Status = %d, want 403", attErr.Outcome.Status)
	}
	if !bytes.Contains(attErr.Body, []byte("access_denied_to_resource")) {
		t.Errorf("AttemptError.Body = %s, want original error body", string(attErr.Body))
	}
}

func TestEgress_Status401Preserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_api_key"}`))
	}))
	defer srv.Close()

	eg := Egress{}
	call := &protocol.Call{
		Op:     OpGet,
		Params: map[string]string{"method": "GET", "path": "/unauth"},
	}
	up := protocol.Upstream{
		BaseURL: srv.URL,
		Secret:  []byte("bearer:expired-key"),
		Client:  srv.Client(),
	}

	_, err := eg.Do(context.Background(), up, call)
	if err == nil {
		t.Fatalf("expected error on 401, got nil")
	}

	var attErr *protocol.AttemptError
	if !errors.As(err, &attErr) {
		t.Fatalf("expected *protocol.AttemptError, got %T: %v", err, err)
	}

	// 401 保持 OutcomeAuthFailed，使调度器隔离该 Key
	if attErr.Outcome.Kind != protocol.OutcomeAuthFailed {
		t.Errorf("Outcome.Kind = %v, want OutcomeAuthFailed", attErr.Outcome.Kind)
	}
	if attErr.Outcome.Status != http.StatusUnauthorized {
		t.Errorf("Outcome.Status = %d, want 401", attErr.Outcome.Status)
	}
}

func TestEgress_NetworkErrorURLSanitized(t *testing.T) {
	// 创建一个在收到连接后立即强制关闭的 TCP 服务器，模拟网络中断
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// 立即关闭底层连接造成连接重置
			_ = conn.Close()
		}
	}()

	secretQueryVal := "super-sensitive-upstream-token-12345"
	eg := Egress{}
	call := &protocol.Call{
		Op: OpGet,
		Params: map[string]string{
			"method": "GET",
			"path":   "/stream",
			"query":  "client_param=secret_client_val",
		},
	}
	up := protocol.Upstream{
		BaseURL: "http://" + l.Addr().String(),
		Secret:  []byte("query:api_key:" + secretQueryVal),
		Client:  http.DefaultClient,
	}

	_, doErr := eg.Do(context.Background(), up, call)
	if doErr == nil {
		t.Fatalf("expected network error, got nil")
	}

	// RS-17: 检查返回的错误信息中绝对不含 query 参数（包括上游凭据与客户端参数）
	errMsg := doErr.Error()
	if strings.Contains(errMsg, secretQueryVal) {
		t.Errorf("upstream secret %q leaked in network error: %s", secretQueryVal, errMsg)
	}
	if strings.Contains(errMsg, "secret_client_val") {
		t.Errorf("client query param leaked in network error: %s", errMsg)
	}
}

func TestEgress_RawBodyPassthrough(t *testing.T) {
	sampleBytes := []byte{0x00, 0xFF, 0x42, 0x13, 0x37}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(sampleBytes)
	}))
	defer srv.Close()

	eg := Egress{}
	call := &protocol.Call{
		Op:     OpGet,
		Params: map[string]string{"method": "GET", "path": "/binary"},
	}
	up := protocol.Upstream{
		BaseURL: srv.URL,
		Secret:  []byte("bearer:token"),
		Client:  srv.Client(),
	}

	resp, err := eg.Do(context.Background(), up, call)
	if err != nil {
		t.Fatalf("unexpected Do error: %v", err)
	}
	defer resp.Raw.Close()

	body, err := io.ReadAll(resp.Raw)
	if err != nil {
		t.Fatalf("failed to read raw body: %v", err)
	}
	if !bytes.Equal(body, sampleBytes) {
		t.Errorf("raw body = %v, want %v", body, sampleBytes)
	}
}
