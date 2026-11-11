package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/core"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// ---------------------------------------------------------------- 已实现部分（可直接运行）

func TestDecodeRequest(t *testing.T) {
	tests := []struct {
		in       string
		wantCode int // 0 = 成功
		notif    bool
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"ping"}`, 0, false},
		{`{"jsonrpc":"2.0","id":"a","method":"tools/list","params":{}}`, 0, false},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 0, true},
		{`{"jsonrpc":"2.0","id":1,"method":`, CodeParseError, false},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, CodeInvalidRequest, false},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, CodeInvalidRequest, false},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, CodeInvalidRequest, false},
		{`{"jsonrpc":"2.0","id":{},"method":"ping"}`, CodeInvalidRequest, false},
		{`{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest, false},
	}
	for _, tt := range tests {
		req, err := DecodeRequest([]byte(tt.in))
		var e *Error
		switch {
		case tt.wantCode == 0 && (err != nil || req.IsNotification() != tt.notif):
			t.Errorf("DecodeRequest(%s) = %+v, %v", tt.in, req, err)
		case tt.wantCode != 0 && (!errors.As(err, &e) || e.Code != tt.wantCode):
			t.Errorf("DecodeRequest(%s) error = %v, want code %d", tt.in, err, tt.wantCode)
		}
	}
	if _, err := DecodeRequest([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); !errors.Is(err, errNotRequest) {
		t.Errorf("client response: error = %v, want errNotRequest", err)
	}
}

func TestFillPath(t *testing.T) {
	got, err := fillPath("/v1/responses/{id}/cancel", map[string]string{"id": "r/1"})
	if err != nil || got != "/v1/responses/r%2F1/cancel" {
		t.Fatalf("fillPath() = %q, %v", got, err)
	}
	if _, err := fillPath("/jobs/{id}", nil); err == nil {
		t.Fatal("missing value accepted")
	}
	if got, _ := fillPath("/search", nil); got != "/search" {
		t.Fatalf("fillPath(no params) = %q", got)
	}
}

// TL-12：{name...} 的值是已转义的路径，原样写入，允许为空。
func TestFillPathWildcard(t *testing.T) {
	ok := []struct{ in, want string }{
		{"repos/a/b", "/rest/s/repos/a/b"},
		{"a%2Fb/c%20d", "/rest/s/a%2Fb/c%20d"},
		{"", "/rest/s/"},
	}
	for _, tt := range ok {
		got, err := fillPath("/rest/{service}/{path...}", map[string]string{"service": "s", "path": tt.in})
		if err != nil || got != tt.want {
			t.Errorf("fillPath(path=%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	if got, err := fillPath("/rest/{service}/{path...}", map[string]string{"service": "s"}); err != nil || got != "/rest/s/" {
		t.Errorf("fillPath(path missing) = %q, %v", got, err)
	}
	for _, bad := range []string{"a?b", "a#b", "a b", "a\tb", "a%zz", "a%2"} {
		if _, err := fillPath("/rest/{service}/{path...}", map[string]string{"service": "s", "path": bad}); err == nil {
			t.Errorf("fillPath(path=%q) accepted", bad)
		}
	}
}

func TestNewInternalRequestWildcardPathValue(t *testing.T) {
	outer := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rt := protocol.Route{Method: http.MethodGet, Path: "/rest/{service}/{path...}"}
	req, err := newInternalRequest(outer, rt, &protocol.ToolRequest{PathValues: map[string]string{"service": "s", "path": "a%2Fb/c"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.URL.EscapedPath(); got != "/rest/s/a%2Fb/c" {
		t.Errorf("EscapedPath = %q", got)
	}
	if got := req.PathValue("path"); got != "a/b/c" {
		t.Errorf("PathValue(path) = %q", got)
	}
}

func TestBufferedWriter(t *testing.T) {
	b := newBufferedWriter(5)
	b.Header().Set("Content-Type", "text/plain")
	if n, err := b.Write([]byte("hello world")); n != 11 || err != nil {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	b.WriteHeader(500) // 已写出后不再改变状态码
	if !b.overflow || b.body.String() != "hello" || b.status != 200 {
		t.Fatalf("overflow=%v body=%q status=%d", b.overflow, b.body.String(), b.status)
	}
	var _ http.Flusher = b
}

func TestNewInternalRequest(t *testing.T) {
	outer := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	outer.Header.Set("Authorization", "Bearer uai_live_secret")
	ctx := obs.WithRequestID(outer.Context(), "req_x")
	outer = outer.WithContext(ctx)
	rt := protocol.Route{Method: http.MethodGet, Path: "/jobs/{id}", Kind: protocol.RouteCall, Op: "get"}
	req, err := newInternalRequest(outer, rt, &protocol.ToolRequest{Op: "get", PathValues: map[string]string{"id": "j1"},
		Header: http.Header{"X-Test": {"1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/jobs/j1" || req.PathValue("id") != "j1" || req.Header.Get("X-Test") != "1" {
		t.Fatalf("request = %s %v", req.URL, req.Header)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("outer Authorization copied into the internal request")
	}
	if obs.RequestIDFrom(req.Context()) != "req_x" {
		t.Fatal("context not inherited")
	}
}

func TestWriteHTTPError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteHTTPError(rec, "req_1", apperr.New(apperr.CodeUnauthenticated, "missing token"))
	var resp Response
	if rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") ||
		json.Unmarshal(rec.Body.Bytes(), &resp) != nil || resp.Error == nil || resp.Error.Message != "missing token" || string(resp.ID) != "null" {
		t.Fatalf("status=%d header=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
}

// ---------------------------------------------------------------- T32：服务端（使用测试协议 echo）

// echo 是测试用协议：POST /echo {"model","text"} → 上游返回 {"echo": text, "units": n}；
// 工具 echo_text（固定服务 "echo-svc"）与 echo_any（服务由参数指定）。
type echoIngress struct{}

func (echoIngress) Protocol() protocol.Protocol { return "echo" }
func (echoIngress) Routes() []protocol.Route {
	return []protocol.Route{{Method: "POST", Path: "/echo", Kind: protocol.RouteCall, Op: "echo"}}
}
func (echoIngress) Claims(*http.Request) bool { return false }
func (echoIngress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	var v struct{ Model string }
	if json.Unmarshal(body, &v) != nil || v.Model == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "model: required")
	}
	return &protocol.Envelope{Protocol: "echo", Op: rt.Op, Model: v.Model, Body: body, Header: r.Header}, nil
}
func (echoIngress) WriteError(w http.ResponseWriter, _ string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]string{"error": err.Message})
}
func (echoIngress) StreamError(err *apperr.Error) sse.Event {
	return sse.Event{Data: []byte(err.Message)}
}
func (echoIngress) WriteCatalog(http.ResponseWriter, []protocol.ModelInfo) {}

var objSchema = json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)

func (echoIngress) Tools() []protocol.Tool {
	return []protocol.Tool{
		{Name: "echo_text", Description: "Echo text back.", InputSchema: objSchema, Model: "echo-svc", ReadOnly: true},
		{Name: "echo_any", Description: "Echo text with a chosen model.", InputSchema: objSchema},
		{Name: "echo_hidden", Description: "Uses a service the client may not use.", InputSchema: objSchema, Model: "secret-svc"},
	}
}
func (echoIngress) ToolRequest(name string, args json.RawMessage) (*protocol.ToolRequest, error) {
	var a struct{ Text, Model string }
	if json.Unmarshal(args, &a) != nil || a.Text == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "text is required")
	}
	model := map[string]string{"echo_text": "echo-svc", "echo_hidden": "secret-svc"}[name]
	if model == "" {
		model = a.Model
	}
	body, _ := json.Marshal(map[string]string{"model": model, "text": a.Text})
	return &protocol.ToolRequest{Op: "echo", Body: body}, nil
}
func (echoIngress) ToolResult(_ string, status int, _ http.Header, body []byte) (*protocol.ToolResult, error) {
	var v struct{ Echo, Error string }
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	if status >= 300 {
		return protocol.TextResult(v.Error, true), nil
	}
	return protocol.TextResult(v.Echo, false), nil
}

type echoEgress struct{}

func (echoEgress) Protocol() protocol.Protocol { return "echo" }
func (echoEgress) Units() []string             { return []string{pricing.UnitRequests} }
func (echoEgress) Prepare(env *protocol.Envelope, _ string) (*protocol.Call, error) {
	return &protocol.Call{Op: env.Op, Body: env.Body, Header: env.Header}, nil
}
func (echoEgress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", protocol.JoinURL(up.BaseURL, "/echo"), strings.NewReader(string(call.Body)))
	req.Header.Set("Authorization", "Bearer "+string(up.Secret))
	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}
	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}
	return &protocol.Response{Kind: protocol.ResponseBuffered, Header: http.Header{"Content-Type": {"application/json"}},
		Body: body, Meter: echoMeter{}}, nil
}

type echoMeter struct{}

func (echoMeter) Usage() (pricing.Usage, protocol.UsageSource) {
	return pricing.Usage{pricing.UnitRequests: 1}, protocol.UsageUpstream
}
func (echoMeter) Fault() (protocol.Outcome, bool) { return protocol.Outcome{}, false }

type testServer struct {
	srv    *Server
	ledger *ledger.Memory
}

func newTestServer(t *testing.T, maxResult int64) *testServer {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&v)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"echo": v.Text})
	}))
	t.Cleanup(up.Close)
	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools: [ { name: e, protocol: echo, base_url: "` + up.URL + `" } ]
models:
  - { name: echo-svc, pool: e, price: { requests: "0.001" } }
  - { name: echo-alt, pool: e, price: { requests: "0.001" } }
  - { name: secret-svc, pool: e, price: { requests: "0.001" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	reg := protocol.NewRegistry()
	if err := reg.RegisterIngress(echoIngress{}, protocol.IngressOptions{Fallback: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterEgress(echoEgress{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Seal(); err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{Logger: obs.Discard()})
	sched.SetKeys([]*scheduler.Key{scheduler.NewKey(1, "e", "k", []byte("sk-e"), 1, 10, scheduler.PersistedReady)})
	led := ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: 100})
	led.LoadAccounts([]ledger.Account{{ClientID: 1, QuotaNanoUSD: 1_000_000_000_000}})
	c, err := core.New(core.Deps{Config: cfg, Registry: reg, Ledger: led, Scheduler: sched, Audit: audit.NewPipeline(10),
		HTTPClient: up.Client(), Logger: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{srv: New(Deps{Registry: reg, Core: c, MaxResultBytes: maxResult, Version: "test", Logger: obs.Discard()}), ledger: led}
}

// rpc 以客户端 1（白名单：echo-svc、echo-alt）的身份 POST 一条消息。
func (ts *testServer) rpc(t *testing.T, body string, h http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, vs := range h {
		req.Header[k] = vs
	}
	ctx := auth.WithPrincipal(req.Context(), &auth.Principal{ClientID: 1, AllowedModels: []string{"echo-svc", "echo-alt"}})
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, req.WithContext(obs.WithRequestID(ctx, "req_mcp")))
	return rec
}

func decodeResult(t *testing.T, rec *httptest.ResponseRecorder, into any) *Response {
	t.Helper()
	var resp Response
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("status=%d type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if into != nil && resp.Result != nil {
		if err := json.Unmarshal(resp.Result, into); err != nil {
			t.Fatal(err)
		}
	}
	return &resp
}

// SRV-M02、SRV-M07：initialize 协商版本
func TestInitialize(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil), &res)
	if res.ProtocolVersion != "2025-03-26" || res.Capabilities.Tools == nil || res.ServerInfo.Name != "universal-ai-gateway" || res.ServerInfo.Version != "test" {
		t.Fatalf("initialize = %+v", res)
	}
	decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil), &res)
	if res.ProtocolVersion != SupportedVersions[0] {
		t.Fatalf("unknown version negotiated to %q, want %q", res.ProtocolVersion, SupportedVersions[0])
	}
}

// SRV-M01、SRV-M03 ~ SRV-M05
func TestTransportRules(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	if rec := ts.rpc(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("notification: status=%d body=%q, want 202 empty", rec.Code, rec.Body.String())
	}
	if rec := ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, http.Header{"Mcp-Protocol-Version": {"1999-01-01"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("unsupported MCP-Protocol-Version: status=%d, want 400", rec.Code)
	}
	resp := decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":7,"method":"ping"}`, nil), nil)
	if string(resp.ID) != "7" || string(resp.Result) != "{}" {
		t.Errorf("ping = %+v", resp)
	}
	if resp := decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"nope"}`, nil), nil); resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Errorf("unknown method = %+v", resp)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		ts.srv.ServeHTTP(rec, httptest.NewRequest(m, "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp status = %d, want 405", m, rec.Code)
		}
	}
	if rec := ts.rpc(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, nil); !strings.Contains(rec.Body.String(), "-32600") {
		t.Errorf("batch: body=%s, want invalid request error", rec.Body.String())
	}
}

// TL-01 ~ TL-04：只列出客户端可用的工具；参数选服务的工具附上可用服务列表
func TestToolsListFiltered(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil), &res)
	if len(res.Tools) != 2 || res.Tools[0].Name != "echo_any" || res.Tools[1].Name != "echo_text" {
		t.Fatalf("tools = %+v, want [echo_any echo_text] (echo_hidden filtered)", res.Tools)
	}
	if !strings.Contains(res.Tools[0].Description, "Available models: echo-alt, echo-svc") {
		t.Errorf("echo_any description = %q", res.Tools[0].Description)
	}
	if !res.Tools[1].Annotations.ReadOnlyHint || !json.Valid(res.Tools[1].InputSchema) {
		t.Errorf("echo_text = %+v", res.Tools[1])
	}
}

// TL-05 ~ TL-10：工具调用走内核（计费）并转换结果
func TestToolsCall(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	var res struct {
		Content []struct{ Type, Text string } `json:"content"`
		IsError bool                          `json:"isError"`
	}
	decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_text","arguments":{"text":"hi"}}}`, nil), &res)
	if res.IsError || len(res.Content) != 1 || res.Content[0].Type != "text" || res.Content[0].Text != "hi" {
		t.Fatalf("echo_text = %+v", res)
	}
	select {
	case ev := <-ts.ledger.Events():
		if ev.Model != "echo-svc" || ev.Usage[pricing.UnitRequests] != 1 {
			t.Fatalf("usage event = %+v", ev)
		}
	default:
		t.Fatal("tool call was not billed")
	}
	// 参数错误、服务不在白名单：工具结果 isError，而不是 JSON-RPC 错误
	for _, call := range []string{
		`{"name":"echo_text","arguments":{}}`,
		`{"name":"echo_hidden","arguments":{"text":"x"}}`,
		`{"name":"echo_any","arguments":{"text":"x","model":"secret-svc"}}`,
	} {
		res.IsError = false
		resp := decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+call+`}`, nil), &res)
		if resp.Error != nil || !res.IsError {
			t.Errorf("%s: resp=%+v result=%+v, want isError result", call, resp, res)
		}
	}
	// 不存在的工具：JSON-RPC 错误
	if resp := decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`, nil), nil); resp.Error == nil || resp.Error.Code != CodeInvalidParams {
		t.Errorf("unknown tool = %+v", resp)
	}
}

// TL-09：结果超过上限
func TestToolsCallResultTooLarge(t *testing.T) {
	ts := newTestServer(t, 8)
	var res struct {
		IsError bool `json:"isError"`
	}
	decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_text","arguments":{"text":"a long enough text"}}}`, nil), &res)
	if !res.IsError {
		t.Fatal("oversized result not reported as error")
	}
}

// SRV-M06: Origin 校验（防 DNS 重绑定）
func TestOriginRejected(t *testing.T) {
	ts := newTestServer(t, 1<<20)

	allowedOrigins := []string{
		"http://localhost",
		"http://localhost:3000",
		"https://localhost:8443",
		"http://127.0.0.1",
		"http://127.0.0.1:8080",
		"http://[::1]",
		"http://[::1]:8080",
	}
	for _, o := range allowedOrigins {
		rec := ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, http.Header{"Origin": {o}})
		if rec.Code != http.StatusOK {
			t.Errorf("origin %q rejected: status=%d", o, rec.Code)
		}
	}

	rejectedOrigins := []string{
		"http://evil.com",
		"https://evil.com:8080",
		"http://localhost.evil.com",
		"http://attacker.com",
		"null",
	}
	for _, o := range rejectedOrigins {
		rec := ts.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, http.Header{"Origin": {o}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("origin %q status = %d, want 403", o, rec.Code)
		}
	}
}

// 外层请求取消时内部请求随之取消
func TestToolsCallCanceledWithClient(t *testing.T) {
	upstreamReached := make(chan struct{})
	upstreamFinished := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(upstreamReached)
		<-r.Context().Done()
		close(upstreamFinished)
	}))
	defer up.Close()

	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools: [ { name: e, protocol: echo, base_url: "` + up.URL + `" } ]
models:
  - { name: echo-svc, pool: e, price: { requests: "0.001" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	reg := protocol.NewRegistry()
	if err := reg.RegisterIngress(echoIngress{}, protocol.IngressOptions{Fallback: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterEgress(echoEgress{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Seal(); err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{Logger: obs.Discard()})
	sched.SetKeys([]*scheduler.Key{scheduler.NewKey(1, "e", "k", []byte("sk-e"), 1, 10, scheduler.PersistedReady)})
	led := ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: 100})
	led.LoadAccounts([]ledger.Account{{ClientID: 1, QuotaNanoUSD: 1_000_000_000_000}})
	c, err := core.New(core.Deps{Config: cfg, Registry: reg, Ledger: led, Scheduler: sched, Audit: audit.NewPipeline(10),
		HTTPClient: up.Client(), Logger: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Registry: reg, Core: c, MaxResultBytes: 1 << 20, Version: "test", Logger: obs.Discard()})

	ctx, cancel := context.WithCancel(context.Background())
	ctx = auth.WithPrincipal(ctx, &auth.Principal{ClientID: 1, AllowedModels: []string{"echo-svc"}})
	ctx = obs.WithRequestID(ctx, "req_cancel")

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_text","arguments":{"text":"hello"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)

	done := make(chan struct{})
	rec := httptest.NewRecorder()
	go func() {
		defer close(done)
		srv.ServeHTTP(rec, req)
	}()

	select {
	case <-upstreamReached:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream to be reached")
	}

	select {
	case <-upstreamFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not observe context cancellation")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not finish after cancellation")
	}
}

// 结果与错误中不出现上游 Key 与网关 Token
func TestNoSecretsInToolOutput(t *testing.T) {
	const upstreamKey = "sk-upstream-secret-12345"
	const gatewayToken = "uai_live_gateway_token_98765"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&v)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"echo": v.Text})
	}))
	defer up.Close()

	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools: [ { name: e, protocol: echo, base_url: "` + up.URL + `" } ]
models:
  - { name: echo-svc, pool: e, price: { requests: "0.001" } }
  - { name: secret-svc, pool: e, price: { requests: "0.001" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	reg := protocol.NewRegistry()
	if err := reg.RegisterIngress(echoIngress{}, protocol.IngressOptions{Fallback: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterEgress(echoEgress{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Seal(); err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{Logger: obs.Discard()})
	sched.SetKeys([]*scheduler.Key{scheduler.NewKey(1, "e", "k", []byte(upstreamKey), 1, 10, scheduler.PersistedReady)})
	led := ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: 100})
	led.LoadAccounts([]ledger.Account{{ClientID: 1, QuotaNanoUSD: 1_000_000_000_000}})
	c, err := core.New(core.Deps{Config: cfg, Registry: reg, Ledger: led, Scheduler: sched, Audit: audit.NewPipeline(10),
		HTTPClient: up.Client(), Logger: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Registry: reg, Core: c, MaxResultBytes: 1 << 20, Version: "test", Logger: obs.Discard()})

	testCases := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_text","arguments":{"text":"normal"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo_text","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo_hidden","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"unknown_method"}`,
	}

	for _, tc := range testCases {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(tc))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+gatewayToken)
		ctx := auth.WithPrincipal(req.Context(), &auth.Principal{ClientID: 1, AllowedModels: []string{"echo-svc"}})
		req = req.WithContext(obs.WithRequestID(ctx, "req_test"))

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		body := rec.Body.String()
		if strings.Contains(body, upstreamKey) {
			t.Errorf("upstream key leaked in body for %s: %s", tc, body)
		}
		if strings.Contains(body, gatewayToken) {
			t.Errorf("gateway token leaked in body for %s: %s", tc, body)
		}
	}
}

// ---------------------------------------------------------------- T33：客户端与 stdio 桥

func TestClientAndBridge(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer uai_live_test" {
			WriteHTTPError(w, "", apperr.New(apperr.CodeUnauthenticated, "bad token"))
			return
		}
		ctx := auth.WithPrincipal(r.Context(), &auth.Principal{ClientID: 1, AllowedModels: []string{"echo-svc"}})
		ts.srv.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer gw.Close()
	c := &Client{URL: gw.URL + "/mcp", Token: "uai_live_test", HTTP: gw.Client()}

	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) == 0 {
		t.Fatalf("ListTools() = %v, %v", tools, err)
	}
	res, err := c.CallTool(context.Background(), "echo_text", json.RawMessage(`{"text":"yo"}`))
	if err != nil || res.IsError || res.Content[0].Text != "yo" {
		t.Fatalf("CallTool() = %+v, %v", res, err)
	}
	bad := &Client{URL: c.URL, Token: "uai_live_wrong", HTTP: gw.Client()}
	if _, err := bad.ListTools(context.Background()); err == nil || strings.Contains(err.Error(), "uai_live_wrong") {
		t.Fatalf("bad token error = %v (must fail and must not contain the token)", err)
	}

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo_text","arguments":{"text":"via stdio"}}}` + "\n")
	var out, log strings.Builder
	if err := Bridge(context.Background(), c, in, &out, &log); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "via stdio") {
		t.Fatalf("bridge output = %q (want 2 lines: initialize and tools/call; no line for the notification)", out.String())
	}
	if strings.Contains(log.String(), "uai_live_test") {
		t.Error("token written to the bridge log")
	}
}

func TestBridgeServerError(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteHTTPError(w, "", apperr.New(apperr.CodeInternal, "internal server failure"))
	}))
	defer gw.Close()

	c := &Client{URL: gw.URL + "/mcp", Token: "uai_live_secret_token_123", HTTP: gw.Client()}

	in := strings.NewReader(`{"jsonrpc":"2.0","id":100,"method":"tools/call","params":{"name":"echo"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/test"}` + "\n" +
		`{"jsonrpc":"2.0","id":"req-abc","method":"ping"}` + "\n")
	var out, log strings.Builder
	if err := Bridge(context.Background(), c, in, &out, &log); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("bridge output lines = %d, want 2: %q", len(lines), out.String())
	}

	// 第一条必须是 id: 100, code: -32603
	var resp1 struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *Error          `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp1); err != nil {
		t.Fatalf("unmarshal resp1 error = %v", err)
	}
	if string(resp1.ID) != "100" || resp1.Error == nil || resp1.Error.Code != CodeInternalError {
		t.Errorf("resp1 = %+v, want id 100, code -32603", resp1)
	}

	// 第二条必须是 id: "req-abc", code: -32603
	var resp2 struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *Error          `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resp2); err != nil {
		t.Fatalf("unmarshal resp2 error = %v", err)
	}
	if string(resp2.ID) != `"req-abc"` || resp2.Error == nil || resp2.Error.Code != CodeInternalError {
		t.Errorf("resp2 = %+v, want id \"req-abc\", code -32603", resp2)
	}

	// 诊断日志不泄露 Token
	if strings.Contains(log.String(), "uai_live_secret_token_123") {
		t.Error("token leaked into bridge log")
	}
	if strings.Contains(out.String(), "uai_live_secret_token_123") {
		t.Error("token leaked into bridge out")
	}
}

func TestClientSSE(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"status\":\"ok\"}}\n\n")
	}))
	defer gw.Close()

	c := &Client{URL: gw.URL + "/mcp", Token: "uai_live_test", HTTP: gw.Client()}
	resp, err := c.Post(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), `"status":"ok"`) {
		t.Fatalf("unexpected sse response: %s", string(resp))
	}
}
