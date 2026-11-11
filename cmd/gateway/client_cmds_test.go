package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

func newFakeMCPGateway(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mcp" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-client-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      nil,
				"error": map[string]any{
					"code":    -32603,
					"message": "unauthorized token",
				},
			})
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities": map[string]any{
						"tools": map[string]any{"listChanged": false},
					},
					"serverInfo": map[string]any{
						"name":    "universal-ai-gateway",
						"version": "test",
					},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "echo_tool",
							"description": "Echo input back\nSecond line of description",
							"inputSchema": map[string]any{"type": "object"},
						},
						{
							"name":        "calc_fail",
							"description": "Calculation failing tool\nWill return isError",
							"inputSchema": map[string]any{"type": "object"},
						},
						{
							"name":        "rich_tool",
							"description": "Returns image and text",
							"inputSchema": map[string]any{"type": "object"},
						},
					},
				},
			})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			w.Header().Set("Content-Type", "application/json")

			switch p.Name {
			case "echo_tool":
				var args struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(p.Arguments, &args)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": "echo: " + args.Text},
						},
						"isError": false,
					},
				})
			case "calc_fail":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": "division by zero error"},
						},
						"isError": true,
					},
				})
			case "rich_tool":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": "some description"},
							{"type": "image", "data": "base64data", "mimeType": "image/png"},
						},
						"isError": false,
					},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error": map[string]any{
						"code":    -32602,
						"message": "unknown tool",
					},
				})
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]any{
					"code":    -32601,
					"message": "method not found",
				},
			})
		}
	}))
}

func testEnv(url, token string) func(string) string {
	return func(k string) string {
		switch k {
		case "UAIG_URL":
			return url
		case "UAIG_TOKEN":
			return token
		default:
			return ""
		}
	}
}

// CLI-06: gateway tools [--json]
func TestCmdTools(t *testing.T) {
	gw := newFakeMCPGateway(t)
	defer gw.Close()

	env := testEnv(gw.URL, "test-client-token")

	// 1. 默认文本输出（名称 + 描述首行）
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"tools"}, strings.NewReader(""), &out, &errb, env)
		if code != 0 {
			t.Fatalf("gateway tools: code = %d, stderr = %s", code, errb.String())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 3 {
			t.Fatalf("expected 3 lines, got %d: %q", len(lines), out.String())
		}
		if !strings.Contains(lines[0], "echo_tool") || !strings.Contains(lines[0], "Echo input back") {
			t.Errorf("line 0 format unexpected: %s", lines[0])
		}
		if strings.Contains(lines[0], "Second line") {
			t.Errorf("line 0 should only contain the first line of description: %s", lines[0])
		}
	}

	// 2. --json 完整 JSON 列表
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"tools", "--json"}, strings.NewReader(""), &out, &errb, env)
		if code != 0 {
			t.Fatalf("gateway tools --json: code = %d, stderr = %s", code, errb.String())
		}
		var tools []mcp.ToolInfo
		if err := json.Unmarshal(out.Bytes(), &tools); err != nil {
			t.Fatalf("unmarshal json tools error = %v, out = %s", err, out.String())
		}
		if len(tools) != 3 {
			t.Fatalf("expected 3 tools, got %d", len(tools))
		}
		if tools[0].Name != "echo_tool" || !strings.Contains(tools[0].Description, "Second line") {
			t.Errorf("tools[0] unexpected: %+v", tools[0])
		}
	}

	// 3. 鉴权失败（错误码 1，不泄露 Token）
	{
		badEnv := testEnv(gw.URL, "uai_live_wrong_secret")
		var out, errb bytes.Buffer
		code := realMainIO([]string{"tools"}, strings.NewReader(""), &out, &errb, badEnv)
		if code != 1 {
			t.Fatalf("gateway tools bad token: code = %d, want 1", code)
		}
		if strings.Contains(errb.String(), "uai_live_wrong_secret") {
			t.Errorf("token leaked in stderr: %s", errb.String())
		}
		if strings.Contains(out.String(), "uai_live_wrong_secret") {
			t.Errorf("token leaked in stdout: %s", out.String())
		}
	}
}

// CLI-07: gateway call <tool> [--args JSON|-] [--json] [--timeout 10m]
func TestCmdCall(t *testing.T) {
	gw := newFakeMCPGateway(t)
	defer gw.Close()

	env := testEnv(gw.URL, "test-client-token")

	// 1. 正常调用
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"call", "echo_tool", "--args", `{"text":"hello"}`}, strings.NewReader(""), &out, &errb, env)
		if code != 0 {
			t.Fatalf("gateway call: code = %d, stderr = %s", code, errb.String())
		}
		if strings.TrimSpace(out.String()) != "echo: hello" {
			t.Errorf("call output = %q, want %q", out.String(), "echo: hello")
		}
	}

	// 2. --args - 从 stdin 读取参数
	{
		var out, errb bytes.Buffer
		stdin := strings.NewReader(`{"text":"from stdin"}`)
		code := realMainIO([]string{"call", "echo_tool", "--args", "-"}, stdin, &out, &errb, env)
		if code != 0 {
			t.Fatalf("gateway call --args -: code = %d, stderr = %s", code, errb.String())
		}
		if strings.TrimSpace(out.String()) != "echo: from stdin" {
			t.Errorf("call stdin output = %q, want %q", out.String(), "echo: from stdin")
		}
	}

	// 3. --json 输出完整结果
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"call", "echo_tool", "--args", `{"text":"json"}`, "--json"}, strings.NewReader(""), &out, &errb, env)
		if code != 0 {
			t.Fatalf("gateway call --json: code = %d, stderr = %s", code, errb.String())
		}
		var res mcp.CallResult
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal json call result: %v", err)
		}
		if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "echo: json" {
			t.Errorf("unexpected call result: %+v", res)
		}
	}

	// 4. isError 时退出码 1
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"call", "calc_fail", "--args", `{}`}, strings.NewReader(""), &out, &errb, env)
		if code != 1 {
			t.Fatalf("gateway call isError: code = %d, want 1", code)
		}
		if strings.TrimSpace(out.String()) != "division by zero error" {
			t.Errorf("call isError output = %q, want %q", out.String(), "division by zero error")
		}
	}

	// 5. 非文本内容被省略提示到 stderr
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"call", "rich_tool", "--args", `{}`}, strings.NewReader(""), &out, &errb, env)
		if code != 0 {
			t.Fatalf("gateway call rich_tool: code = %d, stderr = %s", code, errb.String())
		}
		if strings.TrimSpace(out.String()) != "some description" {
			t.Errorf("rich_tool stdout = %q, want %q", out.String(), "some description")
		}
		if !strings.Contains(errb.String(), "[image content omitted; use --json]") {
			t.Errorf("rich_tool stderr missing image omission notice: %s", errb.String())
		}
	}

	// 6. 工具不存在 / 服务端错误（退出码 1）
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"call", "non_existent_tool", "--args", `{}`}, strings.NewReader(""), &out, &errb, env)
		if code != 1 {
			t.Fatalf("gateway call unknown: code = %d, want 1", code)
		}
		if !strings.Contains(errb.String(), "unknown tool") {
			t.Errorf("stderr missing error message: %s", errb.String())
		}
	}

	// 7. --args 非合法 JSON 对象（退出码 2）
	{
		var out, errb bytes.Buffer
		code := realMainIO([]string{"call", "echo_tool", "--args", `[1, 2, 3]`}, strings.NewReader(""), &out, &errb, env)
		if code != 2 {
			t.Fatalf("gateway call bad args: code = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "--args must be a JSON object") {
			t.Errorf("stderr missing bad args notice: %s", errb.String())
		}
	}
}

// CLI-04: gateway mcp
func TestCmdMCP(t *testing.T) {
	gw := newFakeMCPGateway(t)
	defer gw.Close()

	env := testEnv(gw.URL, "test-client-token")

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo_tool","arguments":{"text":"stdio test"}}}` + "\n")
	var out, errb bytes.Buffer
	code := realMainIO([]string{"mcp"}, in, &out, &errb, env)
	if code != 0 {
		t.Fatalf("gateway mcp: code = %d, stderr = %s", code, errb.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("mcp output lines = %d, want 2: %q", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) {
		t.Errorf("line 0 not initialize: %s", lines[0])
	}
	if !strings.Contains(lines[1], "echo: stdio test") {
		t.Errorf("line 1 not tools/call: %s", lines[1])
	}
}

// newFakeCatalogGateway 模拟目录模式的网关：tools/list 只有目录工具，gateway_search_tools 分页返回 150 个工具。
func newFakeCatalogGateway(t *testing.T, calls *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Limit, Offset int
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": mcp.CatalogCallTool, "description": "c", "inputSchema": map[string]any{"type": "object"}},
				{"name": mcp.CatalogDescribeTool, "description": "d", "inputSchema": map[string]any{"type": "object"}},
				{"name": mcp.CatalogSearchTool, "description": "s", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			*calls = append(*calls, fmt.Sprintf("%s limit=%d offset=%d", req.Params.Name, req.Params.Arguments.Limit, req.Params.Arguments.Offset))
			const total = 150
			var items []map[string]any
			for i := req.Params.Arguments.Offset; i < total && len(items) < req.Params.Arguments.Limit; i++ {
				items = append(items, map[string]any{"name": fmt.Sprintf("tool_%03d", i), "summary": fmt.Sprintf("summary %d", i), "read_only": true})
			}
			off := req.Params.Arguments.Offset
			sc, _ := json.Marshal(map[string]any{"tools": items, "total": total, "offset": off, "has_more": off+len(items) < total})
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "..."}}, "structuredContent": json.RawMessage(sc), "isError": false}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
}

// CLI-08：目录模式下 gateway tools 翻页列出全部真实工具
func TestCmdToolsCatalog(t *testing.T) {
	var calls []string
	gw := newFakeCatalogGateway(t, &calls)
	defer gw.Close()
	env := testEnv(gw.URL, "test-client-token")

	var out, errb bytes.Buffer
	if code := realMainIO([]string{"tools"}, strings.NewReader(""), &out, &errb, env); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 150 || !strings.HasPrefix(lines[0], "tool_000") || !strings.Contains(lines[149], "summary 149") {
		t.Fatalf("got %d lines; first %q", len(lines), lines[0])
	}
	if strings.Join(calls, ";") != "gateway_search_tools limit=100 offset=0;gateway_search_tools limit=100 offset=100" {
		t.Errorf("calls = %v", calls)
	}

	out.Reset()
	if code := realMainIO([]string{"tools", "--json"}, strings.NewReader(""), &out, &errb, env); code != 0 {
		t.Fatalf("--json code = %d", code)
	}
	var tools []struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"read_only"`
	}
	if err := json.Unmarshal(out.Bytes(), &tools); err != nil || len(tools) != 150 || !tools[0].ReadOnly {
		t.Fatalf("--json = %d tools, %v", len(tools), err)
	}
}
