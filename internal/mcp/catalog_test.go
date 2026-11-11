package mcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// withMode 用同一注册表与内核重建服务端，只改目录相关的依赖。
func withMode(ts *testServer, mode string, threshold int, pinned ...string) *testServer {
	d := ts.srv.d
	d.ToolMode, d.AutoThreshold, d.PinnedTools = mode, threshold, pinned
	return &testServer{srv: New(d), ledger: ts.ledger}
}

// rpcAs 以指定客户端身份 POST 一条消息。
func (ts *testServer) rpcAs(t *testing.T, p *auth.Principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithPrincipal(req.Context(), p)
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, req.WithContext(obs.WithRequestID(ctx, "req_cat")))
	return rec
}

func listNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	decodeResult(t, rec, &res)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

const listMsg = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

type callResult struct {
	Content    []struct{ Type, Text string } `json:"content"`
	Structured json.RawMessage               `json:"structuredContent"`
	IsError    bool                          `json:"isError"`
}

func call(t *testing.T, ts *testServer, name, args string) (callResult, *Response) {
	t.Helper()
	var res callResult
	resp := decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`, nil), &res)
	return res, resp
}

func drain(ts *testServer) int {
	n := 0
	for {
		select {
		case <-ts.ledger.Events():
			n++
		default:
			return n
		}
	}
}

// CAT-02、CAT-03：三种模式下的工具列表；auto 按客户端可见工具数切换
func TestCatalogToolsList(t *testing.T) {
	base := newTestServer(t, 1<<20)
	catalogNames := "echo_text,gateway_call_tool,gateway_describe_tool,gateway_search_tools"

	tests := []struct {
		name, mode string
		threshold  int
		pinned     []string
		want       string
	}{
		{"zero value is direct", "", 0, nil, "echo_any,echo_text"},
		{"direct", ToolModeDirect, 0, nil, "echo_any,echo_text"},
		{"catalog without pins", ToolModeCatalog, 0, nil, "gateway_call_tool,gateway_describe_tool,gateway_search_tools"},
		{"catalog with visible and hidden pins", ToolModeCatalog, 0, []string{"echo_text", "echo_hidden"}, catalogNames},
		{"auto at threshold", ToolModeAuto, 2, nil, "echo_any,echo_text"},
		{"auto above threshold", ToolModeAuto, 1, []string{"echo_text"}, catalogNames},
		{"auto default threshold", ToolModeAuto, 0, nil, "echo_any,echo_text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := withMode(base, tt.mode, tt.threshold, tt.pinned...)
			if got := strings.Join(listNames(t, ts.rpc(t, listMsg, nil)), ","); got != tt.want {
				t.Errorf("tools = %s, want %s", got, tt.want)
			}
		})
	}

	// 白名单不同的客户端：可见工具数不同，模式也不同
	ts := withMode(base, ToolModeAuto, 1)
	narrow := &auth.Principal{ClientID: 1, AllowedModels: []string{"echo-svc"}}
	// echo-svc 一个服务：echo_text 与 echo_any（可用服务 echo-svc）仍是 2 个工具 → 目录
	if got := strings.Join(listNames(t, ts.rpcAs(t, narrow, listMsg)), ","); !strings.Contains(got, CatalogSearchTool) {
		t.Errorf("narrow client tools = %s, want catalog", got)
	}
	none := &auth.Principal{ClientID: 1, AllowedModels: []string{"secret-svc"}}
	// 只有 secret-svc：echo_hidden 与 echo_any 共 2 个；阈值放宽到 2 时直接列出
	ts = withMode(base, ToolModeAuto, 2)
	if got := strings.Join(listNames(t, ts.rpcAs(t, none, listMsg)), ","); got != "echo_any,echo_hidden" {
		t.Errorf("secret client tools = %s", got)
	}
}

// CAT-03：只有目录模式在 instructions 中提示目录工具
func TestCatalogInstructions(t *testing.T) {
	base := newTestServer(t, 1<<20)
	initMsg := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	for mode, want := range map[string]bool{ToolModeDirect: false, ToolModeCatalog: true} {
		var res struct{ Instructions string }
		decodeResult(t, withMode(base, mode, 0).rpc(t, initMsg, nil), &res)
		if got := strings.Contains(res.Instructions, CatalogSearchTool); got != want {
			t.Errorf("%s: instructions = %q", mode, res.Instructions)
		}
	}
}

// CAT-04：搜索
func TestCatalogSearch(t *testing.T) {
	ts := withMode(newTestServer(t, 1<<20), ToolModeCatalog, 0)
	type structured struct {
		Tools []struct {
			Name, Summary string
			ReadOnly      bool `json:"read_only"`
		}
		Total   int
		Offset  int
		HasMore bool `json:"has_more"`
	}
	search := func(args string) (callResult, structured) {
		res, resp := call(t, ts, CatalogSearchTool, args)
		if resp.Error != nil {
			t.Fatalf("search(%s) rpc error %+v", args, resp.Error)
		}
		var s structured
		if !res.IsError {
			if err := json.Unmarshal(res.Structured, &s); err != nil {
				t.Fatalf("structured = %s", res.Structured)
			}
		}
		return res, s
	}

	res, s := search(`{}`)
	if s.Total != 2 || len(s.Tools) != 2 || s.Tools[0].Name != "echo_any" || s.Tools[1].Name != "echo_text" || s.HasMore {
		t.Fatalf("empty query = %+v", s)
	}
	if !s.Tools[1].ReadOnly || s.Tools[0].ReadOnly || s.Tools[1].Summary != "Echo text back." {
		t.Errorf("items = %+v", s.Tools)
	}
	if !strings.Contains(res.Content[0].Text, "echo_text — Echo text back.") {
		t.Errorf("text = %q", res.Content[0].Text)
	}
	if _, s := search(`{"query":"ECHO  chosen"}`); s.Total != 1 || s.Tools[0].Name != "echo_any" {
		t.Errorf("multi-word, case-insensitive = %+v", s)
	}
	if _, s := search(`{"query":"echo-alt"}`); s.Total != 1 || s.Tools[0].Name != "echo_any" {
		t.Errorf("match on available-models suffix = %+v", s)
	}
	if res, s := search(`{"query":"hidden"}`); s.Total != 0 || len(s.Tools) != 0 || res.Content[0].Text != "no tools match" {
		t.Errorf("invisible tool leaked: %+v %q", s, res.Content[0].Text)
	}
	if _, s := search(`{"query":"gateway"}`); s.Total != 0 {
		t.Errorf("catalog tools must not be searchable: %+v", s)
	}
	res, s = search(`{"limit":1}`)
	if len(s.Tools) != 1 || !s.HasMore || s.Total != 2 || !strings.Contains(res.Content[0].Text, "offset=1") {
		t.Errorf("page 1 = %+v %q", s, res.Content[0].Text)
	}
	if _, s := search(`{"limit":1,"offset":1}`); len(s.Tools) != 1 || s.Tools[0].Name != "echo_text" || s.HasMore || s.Offset != 1 {
		t.Errorf("page 2 = %+v", s)
	}
	if _, s := search(`{"offset":5}`); s.Total != 2 || len(s.Tools) != 0 || s.HasMore {
		t.Errorf("offset past end = %+v", s)
	}
	for _, bad := range []string{`{"limit":0}`, `{"limit":101}`, `{"offset":-1}`, `{"query":3}`, `{"limit":"x"}`} {
		if res, _ := search(bad); !res.IsError || !strings.Contains(res.Content[0].Text, "invalid arguments") {
			t.Errorf("search(%s) = %+v, want isError", bad, res)
		}
	}
	if n := drain(ts); n != 0 {
		t.Errorf("search produced %d billing events", n)
	}
}

func TestSummaryTruncation(t *testing.T) {
	long := strings.Repeat("字", summaryMaxRunes+5) + "\nsecond line"
	if got := summary(long); got != strings.Repeat("字", summaryMaxRunes)+"…" {
		t.Errorf("summary = %q", got)
	}
	if got := summary("  first  \nsecond"); got != "first" {
		t.Errorf("summary = %q", got)
	}
}

// CAT-05：查看
func TestCatalogDescribe(t *testing.T) {
	ts := withMode(newTestServer(t, 1<<20), ToolModeCatalog, 0)
	res, _ := call(t, ts, CatalogDescribeTool, `{"name":"echo_any"}`)
	var tool struct {
		Name, Description string
		InputSchema       json.RawMessage `json:"inputSchema"`
		Annotations       struct {
			ReadOnlyHint bool `json:"readOnlyHint"`
		} `json:"annotations"`
	}
	if res.IsError || json.Unmarshal(res.Structured, &tool) != nil || tool.Name != "echo_any" ||
		!strings.Contains(tool.Description, "Available models: echo-alt, echo-svc") || !json.Valid(tool.InputSchema) {
		t.Fatalf("describe = %+v / %s", res, res.Structured)
	}
	if res.Content[0].Text != string(res.Structured) {
		t.Errorf("text and structured differ")
	}
	hidden, _ := call(t, ts, CatalogDescribeTool, `{"name":"echo_hidden"}`)
	missing, _ := call(t, ts, CatalogDescribeTool, `{"name":"nope"}`)
	if !hidden.IsError || !missing.IsError || strings.Replace(hidden.Content[0].Text, "echo_hidden", "X", 1) != strings.Replace(missing.Content[0].Text, "nope", "X", 1) {
		t.Errorf("hidden = %+v, missing = %+v: must be indistinguishable", hidden, missing)
	}
	for _, bad := range []string{`{}`, `{"name":""}`, `{"name":1}`} {
		if res, _ := call(t, ts, CatalogDescribeTool, bad); !res.IsError {
			t.Errorf("describe(%s) accepted", bad)
		}
	}
	if n := drain(ts); n != 0 {
		t.Errorf("describe produced %d billing events", n)
	}
}

// CAT-06：调用与直接 tools/call 结果相同，计费一次
func TestCatalogCall(t *testing.T) {
	ts := withMode(newTestServer(t, 1<<20), ToolModeCatalog, 0)
	direct := decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo_text","arguments":{"text":"hi"}}}`, nil), nil)
	if n := drain(ts); n != 1 {
		t.Fatalf("direct call billed %d times", n)
	}
	viaCatalog := decodeResult(t, ts.rpc(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"gateway_call_tool","arguments":{"name":"echo_text","arguments":{"text":"hi"}}}}`, nil), nil)
	if !bytes.Equal(direct.Result, viaCatalog.Result) {
		t.Errorf("results differ:\n direct  %s\n catalog %s", direct.Result, viaCatalog.Result)
	}
	if n := drain(ts); n != 1 {
		t.Errorf("catalog call billed %d times", n)
	}

	cases := map[string]string{
		`{"name":"echo_hidden","arguments":{"text":"x"}}`: "unknown tool echo_hidden",
		`{"name":"nope"}`:                      "unknown tool nope",
		`{"name":"gateway_search_tools"}`:      "cannot call catalog tools",
		`{"name":"echo_text","arguments":"x"}`: "invalid arguments",
		`{"arguments":{}}`:                     "invalid arguments",
		`{"name":"echo_text","arguments":{}}`:  "text is required", // 参数错误由工具自身报告（TL-07）
	}
	for args, want := range cases {
		res, resp := call(t, ts, CatalogCallTool, args)
		if resp.Error != nil || !res.IsError || !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("call(%s) = %+v, want isError containing %q", args, res, want)
		}
	}
	if n := drain(ts); n != 0 {
		t.Errorf("failed calls billed %d times", n)
	}
}

// CAT-07：隐藏不等于禁用；目录工具只在目录模式下可用
func TestCatalogDirectCallRules(t *testing.T) {
	base := newTestServer(t, 1<<20)
	if res, resp := call(t, withMode(base, ToolModeCatalog, 0), "echo_text", `{"text":"hi"}`); resp.Error != nil || res.IsError || res.Content[0].Text != "hi" {
		t.Errorf("hidden tool by name in catalog mode = %+v %+v", res, resp.Error)
	}
	if _, resp := call(t, withMode(base, ToolModeDirect, 0), CatalogSearchTool, `{}`); resp.Error == nil || resp.Error.Code != CodeInvalidParams {
		t.Errorf("catalog tool in direct mode = %+v, want -32602", resp)
	}
}

// CAT-08：日志中没有搜索词与参数
func TestCatalogDoesNotLogArguments(t *testing.T) {
	var buf bytes.Buffer
	ts := withMode(newTestServer(t, 1<<20), ToolModeCatalog, 0)
	d := ts.srv.d
	d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ts = &testServer{srv: New(d), ledger: ts.ledger}
	call(t, ts, CatalogSearchTool, `{"query":"secretword1"}`)
	call(t, ts, CatalogCallTool, `{"name":"echo_text","arguments":{"text":"secretword2"}}`)
	call(t, ts, CatalogCallTool, `{"name":"nope","arguments":{"text":"secretword3"}}`)
	if strings.Contains(buf.String(), "secretword") {
		t.Errorf("log contains arguments: %s", buf.String())
	}
}

type reservedIngress struct{ echoIngress }

func (reservedIngress) Tools() []protocol.Tool {
	return []protocol.Tool{{Name: CatalogCallTool, Description: "clash", InputSchema: objSchema}}
}

// CFG-19、CAT-01
func TestCheckToolConfig(t *testing.T) {
	reg := newTestServer(t, 1<<20).srv.d.Registry
	if err := CheckToolConfig(reg, []string{"echo_text"}); err != nil {
		t.Errorf("valid pins: %v", err)
	}
	for _, pins := range [][]string{{"nope"}, {CatalogSearchTool}} {
		if err := CheckToolConfig(reg, pins); err == nil {
			t.Errorf("pins %v accepted", pins)
		}
	}
	r2 := protocol.NewRegistry()
	if err := r2.RegisterIngress(reservedIngress{}, protocol.IngressOptions{Fallback: true}); err != nil {
		t.Fatal(err)
	}
	if err := r2.RegisterEgress(echoEgress{}); err != nil {
		t.Fatal(err)
	}
	if err := r2.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := CheckToolConfig(r2, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("reserved name: %v", err)
	}
}
