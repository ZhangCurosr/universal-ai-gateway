package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

// syncBuffer 是并发安全的 bytes.Buffer（logger 与测试同时读写 stderr）。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const anthropicStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"up-claude","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// fakeUpstream 同时模拟 Anthropic Messages 与 OpenAI Responses。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 测试桩：读失败时下面的断言会失败
		switch r.URL.Path {
		case "/v1/messages":
			if bytes.Contains(body, []byte(`"stream":true`)) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicStream)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"up-claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
		case "/v1/responses":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"CAP says pick two"}]}],"usage":{"input_tokens":9,"output_tokens":6}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type gateway struct {
	data, admin string // host:port
	adminToken  string
	cfg         *config.Config
	cancel      context.CancelFunc
	done        chan error
}

// startGateway 在临时目录中写配置并启动 run；mutate 可以在 Load 之后改配置。
func startGateway(t *testing.T, upstreamURL string, mutate func(*config.Config)) *gateway {
	t.Helper()
	dir := t.TempDir()
	yaml := fmt.Sprintf(`server:
  data_listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:0"
  data_dir: %q
  master_key_file: %q
  shutdown_grace: 5s
  log_level: warn
pools:
  - name: anth
    protocol: anthropic
    base_url: %q
  - name: oai
    protocol: openai_responses
    base_url: %q
models:
  - name: claude-test
    pool: anth
    upstream_model: up-claude
    price: { input_tokens: "3/1M", output_tokens: "15/1M", cache_read_tokens: "0.3/1M", cache_write_tokens: "3.75/1M" }
    reserve: { output_tokens: 1024 }
  - name: gpt-test
    pool: oai
    price: { input_tokens: "1.25/1M", output_tokens: "10/1M", cache_read_tokens: "0.125/1M" }
    reserve: { output_tokens: 1024 }
`, filepath.ToSlash(filepath.Join(dir, "data")), filepath.ToSlash(filepath.Join(dir, "secrets", "master.key")), upstreamURL, upstreamURL)
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if mutate != nil {
		mutate(cfg)
	}

	stderr := &syncBuffer{}
	type addrs struct{ data, admin net.Addr }
	ready := make(chan addrs, 1)
	ctx, cancel := context.WithCancel(context.Background())
	g := &gateway{cfg: cfg, cancel: cancel, done: make(chan error, 1)}
	go func() {
		g.done <- run(ctx, cfg, runOptions{Stderr: stderr, OnListening: func(d, a net.Addr) { ready <- addrs{d, a} }})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-g.done:
		case <-time.After(10 * time.Second):
			t.Error("gateway did not stop")
		}
	})

	select {
	case a := <-ready:
		g.data, g.admin = a.data.String(), a.admin.String()
	case err := <-g.done:
		t.Fatalf("run returned before listening: %v\nstderr:\n%s", err, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway did not start\nstderr:\n%s", stderr.String())
	}

	const prefix = "BOOTSTRAP ADMIN TOKEN (shown once): "
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			g.adminToken = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	if g.adminToken == "" {
		t.Fatalf("bootstrap token not found in stderr:\n%s", stderr.String())
	}
	return g
}

// doJSON 发请求并把 JSON 响应解码到 out（out 可为 nil）。返回状态码。
func doJSON(t *testing.T, method, url, token string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, url, raw, err)
		}
	}
	if resp.StatusCode >= 300 {
		t.Logf("%s %s -> %d %s", method, url, resp.StatusCode, raw)
	}
	return resp.StatusCode
}

func TestE2E(t *testing.T) {
	up := fakeUpstream(t)
	g := startGateway(t, up.URL, nil)
	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data

	// 1. 管理面：创建客户端与两把 Key
	var created struct {
		Client struct {
			ID int64 `json:"id"`
		} `json:"client"`
		Token string `json:"token"`
	}
	if code := doJSON(t, "POST", adminURL+"/clients", g.adminToken,
		map[string]any{"name": "e2e", "role": "user", "quota_nano_usd": int64(1e12)}, &created); code != http.StatusCreated {
		t.Fatalf("create client: %d", code)
	}
	for _, pool := range []string{"anth", "oai"} {
		if code := doJSON(t, "POST", adminURL+"/keys", g.adminToken,
			map[string]any{"pool": pool, "name": pool + "-k1", "secret": "sk-test-" + pool}, nil); code != http.StatusCreated {
			t.Fatalf("create key for %s: %d", pool, code)
		}
	}

	// 2. 数据面：非流式
	var msg struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if code := doJSON(t, "POST", dataURL+"/v1/messages", created.Token, map[string]any{
		"model": "claude-test", "max_tokens": 16,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, &msg); code != http.StatusOK || len(msg.Content) == 0 || msg.Content[0].Text != "ok" {
		t.Fatalf("non-stream: code=%d body=%+v", code, msg)
	}

	// 3. 数据面：流式
	b, _ := json.Marshal(map[string]any{ // 字面量 map，Marshal 不会失败
		"model": "claude-test", "max_tokens": 16, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest("POST", dataURL+"/v1/messages", bytes.NewReader(b)) // URL 固定合法
	req.Header.Set("Authorization", "Bearer "+created.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var sawStop bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "message_stop") {
			sawStop = true
		}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !sawStop {
		t.Fatalf("stream: code=%d sawStop=%v", resp.StatusCode, sawStop)
	}

	// 4. MCP：tools/list 与 ask_model
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mc := &mcp.Client{URL: dataURL + "/mcp", Token: created.Token, HTTP: http.DefaultClient}
	tools, err := mc.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var hasAsk bool
	for _, tl := range tools {
		hasAsk = hasAsk || tl.Name == "ask_model"
	}
	if !hasAsk {
		t.Fatalf("ask_model not in tools: %+v", tools)
	}
	res, err := mc.CallTool(ctx, "ask_model", json.RawMessage(`{"model":"gpt-test","input":"CAP?"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "CAP says pick two") {
		t.Fatalf("ask_model result = %+v", res)
	}

	// 5. 客户端 spent > 0（Settle 在 handler 收尾时完成，稍等片刻）
	deadline := time.Now().Add(3 * time.Second)
	for {
		var view struct {
			Client struct {
				Spent int64 `json:"spent_nano_usd"`
			} `json:"client"`
		}
		doJSON(t, "GET", fmt.Sprintf("%s/clients/%d", adminURL, created.Client.ID), g.adminToken, nil, &view)
		if view.Client.Spent > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client spent_nano_usd is still 0")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 6. 关闭：进程干净退出，计费事件全部落库
	g.cancel()
	select {
	case err := <-g.done:
		if err != nil {
			t.Fatalf("run() = %v, want nil", err)
		}
		g.done <- nil // 让 Cleanup 不再等待
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not stop")
	}

	db, err := sqlite.Open(context.Background(), filepath.Join(g.cfg.Server.DataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sums, err := db.QueryUsage(context.Background(), store.UsageQuery{ClientID: created.Client.ID})
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, s := range sums {
		requests += s.Requests
	}
	if requests != 3 {
		t.Fatalf("usage_events for client = %d, want 3 (summaries %+v)", requests, sums)
	}
}

// SRV-01：慢速请求头的连接必须被 read_header_timeout 关闭。
func TestSlowHeaderClosed(t *testing.T) {
	up := fakeUpstream(t)
	g := startGateway(t, up.URL, func(c *config.Config) {
		c.Server.ReadHeaderTimeout = 200 * time.Millisecond
	})
	conn, err := net.Dial("tcp", g.data)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n"); err != nil { // 只发半个请求头
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = io.ReadAll(conn) // 服务端关闭连接时返回（可能带一个 408 响应）
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("connection still open after %v", time.Since(start))
	}
}
