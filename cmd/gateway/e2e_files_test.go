package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

// T69：上传一次得到文件 ID；工具（声明了 file 参数）与直接 HTTP 请求都用 ID 代替请求体，
// 经过 T67 的上传 flow 到达假存储，字节与原文件相同；文件只属于上传它的客户端。
func TestE2E_FileRelay(t *testing.T) {
	var mu sync.Mutex
	var puts [][]byte
	var urlReqs []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/urls":
			b, _ := io.ReadAll(r.Body)
			urlReqs = append(urlReqs, string(b))
			_, _ = io.WriteString(w, `{"data":{"url":"http://`+r.Host+`/put/1","id":"t1"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/put/1":
			b, _ := io.ReadAll(r.Body)
			puts = append(puts, b)
		case r.URL.Path == "/task/t1":
			_, _ = io.WriteString(w, `{"data":{"state":"done","pages":2}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer upstream.Close()

	g := startRESTGateway(t, func(c *config.Config) {
		c.Files.Enabled = true
		c.Pools = append(c.Pools, config.PoolConfig{Name: "fr", Protocol: "rest", BaseURL: upstream.URL, MaxAttempts: 3})
		c.Models = append(c.Models, config.ModelConfig{
			Name: "fr-api", Pool: "fr", Price: map[string]string{"pages": "0.01", "requests": "0"}, Reserve: map[string]int64{"pages": 10},
			Tools: []config.ToolConfig{{
				Name: "fr_parse_file", Description: "parse an uploaded file", Method: "POST", Path: "/parse/file",
				File: &config.ToolFile{Arg: "file", NameQuery: "name"},
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"file": map[string]any{"type": "string"}, "lang": map[string]any{"type": "string"}}, "required": []any{"file"}},
			}},
			API: &config.APIConfig{Endpoints: []config.APIEndpoint{{
				Match: "POST /parse/file",
				Usage: map[string]config.APIUsage{"pages": {JSON: "data.pages"}},
				Flow: &config.APIFlow{
					Upload: &config.APIFlowUpload{Request: "POST /urls", UploadURL: "data.url", TaskID: "data.id",
						Body: map[string]config.APIBodyField{"files.0.name": {Query: "name", Required: true}, "lang": {Query: "lang"}}},
					Poll: &config.APIFlowPoll{Request: "GET /task/{task_id}", State: "data.state", Done: []string{"done"}},
				},
			}}},
		})
		c.Normalize()
	})
	admin := "http://" + g.admin + "/admin/api"
	data := "http://" + g.data
	token := func(name string) string {
		var cl struct {
			Token string `json:"token"`
		}
		doJSON(t, "POST", admin+"/clients", g.adminToken, map[string]any{"name": name, "role": "user", "quota_nano_usd": int64(1e12)}, &cl)
		return cl.Token
	}
	tok, otherTok := token("fr"), token("other")
	doJSON(t, "POST", admin+"/keys", g.adminToken, map[string]any{"pool": "fr", "name": "k1", "secret": "fr1-secret-aaaa"}, nil)

	dir := t.TempDir()
	content := []byte("%PDF-1.4 two pages\x00\x01binary")
	local := filepath.Join(dir, "报告 v2.pdf")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mc := &mcp.Client{URL: data + "/mcp", Token: tok, HTTP: http.DefaultClient}
	info, err := mc.UploadFile(ctx, local, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.ID, "file_") || info.Size != int64(len(content)) || info.Name != "报告 v2.pdf" || info.ContentType != "application/pdf" {
		t.Fatalf("upload = %+v", info)
	}

	// 工具调用：file 参数 → 请求体，name_query → ?name=，其余参数进查询串
	res, err := mc.CallTool(ctx, "fr_parse_file", json.RawMessage(`{"file":"`+info.ID+`","lang":"en"}`))
	if err != nil || res.IsError || !strings.Contains(res.Content[0].Text, `"done"`) {
		t.Fatalf("tool call: %+v %v", res, err)
	}
	mu.Lock()
	if len(puts) != 1 || string(puts[0]) != string(content) || len(urlReqs) != 1 || !strings.Contains(urlReqs[0], `"name":"报告 v2.pdf"`) || !strings.Contains(urlReqs[0], `"lang":"en"`) {
		t.Fatalf("upstream saw puts=%d urls=%q", len(puts), urlReqs)
	}
	mu.Unlock()

	do := func(method, path, tk, fileID, body string) (int, string) {
		req, _ := http.NewRequest(method, data+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tk)
		if fileID != "" {
			req.Header.Set("X-UAIG-File", fileID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// 直接 HTTP：同一个 ID 再用一次，不需要重新上传
	if st, body := do("POST", "/rest/fr-api/parse/file?name=again.pdf", tok, info.ID, ""); st != 200 || !strings.Contains(body, "done") {
		t.Fatalf("direct: %d %s", st, body)
	}
	mu.Lock()
	if len(puts) != 2 || string(puts[1]) != string(content) || !strings.Contains(urlReqs[1], "again.pdf") {
		t.Fatalf("direct upstream: puts=%d urls=%q", len(puts), urlReqs)
	}
	mu.Unlock()
	if st, body := do("POST", "/rest/fr-api/parse/file?name=a.pdf", tok, info.ID, "not empty"); st != 400 || !strings.Contains(body, "must be empty") {
		t.Fatalf("own body: %d %s", st, body)
	}
	if st, body := do("GET", "/rest/fr-api/x", tok, info.ID, ""); st != 400 || !strings.Contains(body, "without a request body") {
		t.Fatalf("BodyNone: %d %s", st, body)
	}
	// FS-02：其他客户端看不到、用不了
	if st, _ := do("POST", "/rest/fr-api/parse/file?name=a.pdf", otherTok, info.ID, ""); st != 404 {
		t.Fatalf("other client direct: %d", st)
	}
	other := &mcp.Client{URL: data + "/mcp", Token: otherTok, HTTP: http.DefaultClient}
	if res, err := other.CallTool(ctx, "fr_parse_file", json.RawMessage(`{"file":"`+info.ID+`"}`)); err != nil || !res.IsError || !strings.Contains(res.Content[0].Text, "file expired or not found") {
		t.Fatalf("other client tool: %+v %v", res, err)
	}
	if st, _ := do("GET", "/files/"+info.ID, otherTok, "", ""); st != 404 {
		t.Fatalf("other client get: %d", st)
	}
	if res, err := mc.CallTool(ctx, "fr_parse_file", json.RawMessage(`{"lang":"en"}`)); err != nil || !res.IsError || !strings.Contains(res.Content[0].Text, `argument "file"`) {
		t.Fatalf("missing file arg: %+v %v", res, err)
	}

	// 上传的限制
	if st, body := do("POST", "/files?name=a.txt&ttl=1000h", tok, "", "x"); st != 400 || !strings.Contains(body, "ttl") {
		t.Fatalf("ttl: %d %s", st, body)
	}
	if st, body := do("POST", "/files", tok, "", "x"); st != 400 || !strings.Contains(body, "name") {
		t.Fatalf("no name: %d %s", st, body)
	}
	if st, _ := do("GET", "/files", "uai_live_wrong", "", ""); st != 401 {
		t.Fatalf("unauthenticated: %d", st)
	}

	// 列表、管理视图、删除
	var list struct {
		Files []struct{ ID string } `json:"files"`
		Used  struct{ Files int }   `json:"used"`
	}
	doJSON(t, "GET", data+"/files", tok, nil, &list)
	if len(list.Files) != 1 || list.Files[0].ID != info.ID || list.Used.Files != 1 {
		t.Fatalf("list = %+v", list)
	}
	var av struct {
		Enabled bool `json:"enabled"`
		Used    struct {
			Files int   `json:"files"`
			Bytes int64 `json:"bytes"`
		} `json:"used"`
		Expiring []map[string]any `json:"expiring"`
	}
	doJSON(t, "GET", admin+"/files", g.adminToken, nil, &av)
	if !av.Enabled || av.Used.Files != 1 || av.Used.Bytes != int64(len(content)) || len(av.Expiring) != 1 || av.Expiring[0]["name"] != nil {
		t.Fatalf("admin view = %+v", av)
	}
	if err := mc.DeleteFile(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := do("POST", "/rest/fr-api/parse/file?name=a.pdf", tok, info.ID, ""); st != 404 {
		t.Fatalf("after delete: %d", st)
	}
	if strings.Contains(g.stderr.String(), "报告") {
		t.Fatal("file name written to the log")
	}
}

// files.enabled 为 false 时没有 /files，带 X-UAIG-File 的请求 → 400。
func TestE2E_FileRelayDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer upstream.Close()
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "fd", Protocol: "rest", BaseURL: upstream.URL})
		c.Models = append(c.Models, config.ModelConfig{Name: "fd-api", Pool: "fd", Price: map[string]string{"requests": "0"}})
		c.Normalize()
	})
	var cl struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", "http://"+g.admin+"/admin/api/clients", g.adminToken, map[string]any{"name": "fd", "role": "user", "quota_unlimited": true}, &cl)
	req, _ := http.NewRequest(http.MethodPost, "http://"+g.data+"/rest/fd-api/x", nil)
	req.Header.Set("Authorization", "Bearer "+cl.Token)
	req.Header.Set("X-UAIG-File", "file_aaaaaaaaaaaaaaaaaaaaaaaaaa")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // 测试：关闭失败无影响
	if resp.StatusCode != 400 || !strings.Contains(string(b), "files.enabled") {
		t.Fatalf("disabled: %d %s", resp.StatusCode, b)
	}
	if st := doJSON(t, "GET", "http://"+g.data+"/files", cl.Token, nil, nil); st != 404 {
		t.Fatalf("/files without files.enabled: %d", st)
	}
	var av struct {
		Enabled bool `json:"enabled"`
	}
	doJSON(t, "GET", "http://"+g.admin+"/admin/api/files", g.adminToken, nil, &av)
	if av.Enabled {
		t.Fatal("admin view says enabled")
	}
}
