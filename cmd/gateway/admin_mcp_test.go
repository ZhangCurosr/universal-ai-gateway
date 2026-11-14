package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

type rpcResult struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolCallResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
	IsError    bool            `json:"isError"`
}

func adminRPC(t *testing.T, g *restGateway, token, body string) (int, rpcResult) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+g.admin+"/admin/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out rpcResult
	_ = json.Unmarshal(raw, &out) // 非 JSON（例如 403 的错误体）时由状态码判断
	return resp.StatusCode, out
}

func adminCall(t *testing.T, g *restGateway, name, args string) toolCallResult {
	t.Helper()
	code, out := adminRPC(t, g, g.adminToken, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	if code != 200 || out.Error != nil {
		t.Fatalf("%s: HTTP %d %+v", name, code, out.Error)
	}
	var res toolCallResult
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func adminGateway(t *testing.T) *restGateway {
	return startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "am-pool", Protocol: "rest", BaseURL: "http://127.0.0.1:1"})
		c.Models = append(c.Models, config.ModelConfig{Name: "am-svc", Pool: "am-pool", Price: map[string]string{"requests": "0"},
			Tools: []config.ToolConfig{{Name: "am_lookup", Description: "Look up a thing.\nSecond line.", Method: "GET", Path: "/x"}}})
		c.Normalize()
	})
}

// ADM-21、AM-01 ~ AM-05：管理面 MCP
func TestAdminMCP(t *testing.T) {
	g := adminGateway(t)
	if code := doJSON(t, "POST", "http://"+g.admin+"/admin/api/keys", g.adminToken,
		map[string]any{"pool": "am-pool", "name": "k1", "secret": "bearer:am-secret-value-123"}, nil); code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}
	var created struct {
		Client struct {
			ID int64 `json:"id"`
		} `json:"client"`
		Token string `json:"token"`
	}
	doJSON(t, "POST", "http://"+g.admin+"/admin/api/clients", g.adminToken, map[string]any{"name": "u1", "role": "user", "quota_nano_usd": 1000}, &created)

	code, out := adminRPC(t, g, g.adminToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if code != 200 || !strings.Contains(string(out.Result), `"universal-ai-gateway-admin"`) || !strings.Contains(string(out.Result), "gateway keys import") || !strings.Contains(string(out.Result), `"2025-03-26"`) {
		t.Fatalf("initialize: %d %s", code, out.Result)
	}
	_, out = adminRPC(t, g, g.adminToken, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnly bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out.Result, &list); err != nil || len(list.Tools) != 11 { // T64 新增 gateway_admin_alerts（ADM-28）
		t.Fatalf("tools/list = %s", out.Result)
	}
	ro := 0
	for _, tl := range list.Tools {
		if tl.Annotations.ReadOnly {
			ro++
		}
		for _, banned := range []string{"create", "rotate", "add_key", "token"} {
			if strings.Contains(tl.Name, banned) {
				t.Errorf("credential-producing tool exposed: %s", tl.Name)
			}
		}
	}
	if ro != 8 { // T64：gateway_admin_alerts 是只读工具
		t.Errorf("read-only tools = %d, want 8", ro)
	}

	// 只读工具
	if res := adminCall(t, g, "gateway_admin_status", `{}`); res.IsError || !strings.Contains(res.Content[0].Text, "version") {
		t.Errorf("status = %+v", res)
	}
	res := adminCall(t, g, "gateway_admin_list_keys", `{}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, `"am-pool"`) || strings.Contains(res.Content[0].Text, "am-secret-value-123") {
		t.Fatalf("list_keys = %+v", res)
	}
	var keys struct {
		Keys []struct {
			ID   int64  `json:"id"`
			Pool string `json:"pool"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(res.Structured, &keys); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	for _, k := range keys.Keys {
		if k.Pool == "am-pool" {
			keyID = k.ID
		}
	}
	if res := adminCall(t, g, "gateway_admin_list_clients", `{}`); res.IsError || !strings.Contains(res.Content[0].Text, `"u1"`) || strings.Contains(res.Content[0].Text, created.Token) {
		t.Errorf("list_clients = %+v", res)
	}
	if res := adminCall(t, g, "gateway_admin_usage", `{"client":"u1"}`); res.IsError {
		t.Errorf("usage = %+v", res)
	}
	if res := adminCall(t, g, "gateway_admin_usage", `{"from":"yesterday"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "HTTP 400") {
		t.Errorf("bad usage time = %+v", res)
	}
	if res := adminCall(t, g, "gateway_admin_config", `{}`); res.IsError || !strings.Contains(res.Content[0].Text, `"am-svc"`) {
		t.Errorf("config = %+v", res)
	}
	res = adminCall(t, g, "gateway_admin_tools", `{}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, `"am_lookup"`) || !strings.Contains(res.Content[0].Text, `"Look up a thing."`) ||
		!strings.Contains(res.Content[0].Text, `"tavily_search"`) || strings.Contains(res.Content[0].Text, "Second line") {
		t.Errorf("tools = %+v", res)
	}
	if res := adminCall(t, g, "gateway_admin_alerts", `{}`); res.IsError || !strings.Contains(res.Content[0].Text, `"alerts":[]`) { // ADM-28（T64）
		t.Fatalf("alerts = %+v", res)
	}
	if res := adminCall(t, g, "gateway_admin_reload_records", `{}`); res.IsError || !strings.Contains(res.Content[0].Text, "records") {
		t.Errorf("reload_records = %+v", res)
	}
	// startRESTGateway 没有配置文件路径：重载不可用，以工具错误返回
	if res := adminCall(t, g, "gateway_admin_reload", `{"dry_run":true}`); !res.IsError || !strings.Contains(res.Content[0].Text, "HTTP 404") {
		t.Errorf("reload without config path = %+v", res)
	}

	// 写工具
	if res := adminCall(t, g, "gateway_admin_set_key_state", fmt.Sprintf(`{"id":%d,"state":"disabled"}`, keyID)); res.IsError || !strings.Contains(res.Content[0].Text, `"disabled"`) {
		t.Fatalf("disable key = %+v", res)
	}
	if res := adminCall(t, g, "gateway_admin_set_key_state", fmt.Sprintf(`{"id":%d,"state":"ready"}`, keyID)); res.IsError || !strings.Contains(res.Content[0].Text, `"ready"`) {
		t.Fatalf("enable key = %+v", res)
	}
	res = adminCall(t, g, "gateway_admin_update_client", fmt.Sprintf(`{"id":%d,"rpm_limit":7,"allowed_models":["am-svc"]}`, created.Client.ID))
	if res.IsError || !strings.Contains(res.Content[0].Text, `"rpm_limit":7`) || !strings.Contains(res.Content[0].Text, `"am-svc"`) || !strings.Contains(res.Content[0].Text, `"quota_nano_usd":1000`) {
		t.Fatalf("update client = %+v", res)
	}

	// 参数与错误
	for name, args := range map[string]string{
		"gateway_admin_set_key_state": `{"id":"x","state":"ready"}`,
		"gateway_admin_update_client": `{"id":1,"bogus":true}`,
		"gateway_admin_usage":         `{"client":5}`,
	} {
		if res := adminCall(t, g, name, args); !res.IsError || !strings.Contains(res.Content[0].Text, "invalid arguments") {
			t.Errorf("%s(%s) = %+v", name, args, res)
		}
	}
	if res := adminCall(t, g, "gateway_admin_set_key_state", `{"id":999999,"state":"ready"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "HTTP 404") {
		t.Errorf("unknown key = %+v", res)
	}
	if _, out := adminRPC(t, g, g.adminToken, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`); out.Error == nil || out.Error.Code != -32602 {
		t.Errorf("unknown tool = %+v", out)
	}

	// 鉴权：普通客户端 Token → 403；GET → 405
	if code, _ := adminRPC(t, g, created.Token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != http.StatusForbidden {
		t.Errorf("user token: HTTP %d, want 403", code)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+g.admin+"/admin/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+g.adminToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /admin/mcp = %v %v", resp.StatusCode, err)
	} else {
		resp.Body.Close()
	}

	log := g.stderr.String()
	if !strings.Contains(log, `"admin mcp call"`) || strings.Contains(log, `"rpm_limit"`) || strings.Contains(log, "am-secret-value-123") {
		t.Errorf("admin mcp log: %s", log)
	}

	// CLI-10：gateway admin-mcp 桥接
	env := func(k string) string {
		return map[string]string{"UAIG_ADMIN_URL": "http://" + g.admin, "UAIG_ADMIN_TOKEN": g.adminToken}[k]
	}
	var stdout, stderr bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"gateway_admin_config","arguments":{}}}` + "\n")
	if code := realMainIO([]string{"admin-mcp"}, in, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), "am-svc") {
		t.Fatalf("admin-mcp bridge: %d %q %q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), g.adminToken) {
		t.Error("bridge output leaks the admin token")
	}
}
