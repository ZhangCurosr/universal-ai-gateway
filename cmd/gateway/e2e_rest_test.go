package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
)

type keyViewItem struct {
	ID              int64  `json:"id"`
	Pool            string `json:"pool"`
	Name            string `json:"name"`
	SecretHint      string `json:"secret_hint"`
	State           string `json:"state"`
	StateReason     string `json:"state_reason"`
	CooldownSeconds int    `json:"cooldown_seconds"`
}

type restGateway struct {
	data, admin string // host:port
	adminToken  string
	cfg         *config.Config
	cancel      context.CancelFunc
	done        chan error
	stderr      *syncBuffer
}

func startRESTGateway(t *testing.T, mutate func(*config.Config)) *restGateway {
	t.Helper()
	dir := t.TempDir()
	yaml := fmt.Sprintf(`server:
  data_listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:0"
  data_dir: %q
  master_key_file: %q
  shutdown_grace: 5s
  log_level: info
ledger:
  flush_interval: 20ms
pools:
  - name: base-dummy
    protocol: rest
    base_url: "http://127.0.0.1:1"
models:
  - name: base-dummy-model
    pool: base-dummy
    price: { requests: "0.01" }
    reserve: { requests: 1 }
`, filepath.ToSlash(filepath.Join(dir, "data")), filepath.ToSlash(filepath.Join(dir, "secrets", "master.key")))
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
	origDefaultLogger := slog.Default()
	slog.SetDefault(obs.NewLogger(stderr, "info"))
	t.Cleanup(func() {
		slog.SetDefault(origDefaultLogger)
	})

	type addrs struct{ data, admin net.Addr }
	ready := make(chan addrs, 1)
	ctx, cancel := context.WithCancel(context.Background())
	g := &restGateway{cfg: cfg, cancel: cancel, done: make(chan error, 1), stderr: stderr}
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

// 1. 测试 GET 请求透明转发与按次计费（requests: 1）。
func TestE2E_REST_GET_TransparentForwardingAndBilling(t *testing.T) {
	var receivedAuth, receivedTrace, receivedUA, receivedQuery string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedTrace = r.Header.Get("X-Request-Trace")
		receivedUA = r.Header.Get("User-Agent")
		receivedQuery = r.URL.RawQuery

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", "\"v42\"")
		w.Header().Set("RateLimit-Remaining", "99")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"forecast":"sunny","city":"Beijing"}`)
	}))
	t.Cleanup(upstream.Close)

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:     "weather-pool",
			Protocol: "rest",
			BaseURL:  upstream.URL,
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "weather",
			Pool:    "weather-pool",
			Price:   map[string]string{"requests": "0.01"},
			Reserve: map[string]int64{"requests": 1},
		})
		c.Normalize()
	})

	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data

	// 创建客户端与 Key
	var created struct {
		Client struct {
			ID int64 `json:"id"`
		} `json:"client"`
		Token string `json:"token"`
	}
	if code := doJSON(t, "POST", adminURL+"/clients", g.adminToken,
		map[string]any{"name": "rest-user", "role": "user", "quota_nano_usd": int64(1e12)}, &created); code != http.StatusCreated {
		t.Fatalf("create client: %d", code)
	}

	if code := doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "weather-pool", "name": "weather-k1", "secret": "bearer:test-upstream-secret-123"}, nil); code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}

	// 客户端发 GET 请求
	req, err := http.NewRequest(http.MethodGet, dataURL+"/rest/weather/forecast?city=Beijing&days=3", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)
	req.Header.Set("X-UAIG-Forward-X-Request-Trace", "trace-abc-123")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /rest/weather/forecast: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag != "\"v42\"" {
		t.Errorf("ETag = %q, want %q", etag, "\"v42\"")
	}
	if rl := resp.Header.Get("RateLimit-Remaining"); rl != "99" {
		t.Errorf("RateLimit-Remaining = %q, want 99", rl)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sunny") {
		t.Errorf("body = %s, want to contain 'sunny'", string(body))
	}

	// 校验上游接收到的数据
	if receivedAuth != "Bearer test-upstream-secret-123" {
		t.Errorf("upstream auth = %q, want Bearer test-upstream-secret-123", receivedAuth)
	}
	if receivedTrace != "trace-abc-123" {
		t.Errorf("upstream X-Request-Trace = %q, want trace-abc-123", receivedTrace)
	}
	if receivedUA == "" {
		t.Errorf("upstream User-Agent is empty")
	}
	if receivedQuery != "city=Beijing&days=3" {
		t.Errorf("upstream query = %q, want city=Beijing&days=3", receivedQuery)
	}

	// 校验计费已结算（requests: 1 扣费）
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
			t.Fatal("client spent_nano_usd is still 0 after requests: 1")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 2. 测试 POST 请求（验证只尝试 1 次，上游 500 不重试；对比 /rest-idem/ 重试）。
func TestE2E_REST_POST_SingleAttemptNoRetry(t *testing.T) {
	var postAttempts atomic.Int32
	var idemAttempts atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/post") {
			postAttempts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"server_error"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/idem") {
			idemAttempts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"server_error"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:        "notify-pool",
			Protocol:    "rest",
			BaseURL:     upstream.URL,
			MaxAttempts: 3,
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "notify",
			Pool:    "notify-pool",
			Price:   map[string]string{"requests": "0.01"},
			Reserve: map[string]int64{"requests": 1},
		})
		c.Normalize()
	})

	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data

	var created struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", adminURL+"/clients", g.adminToken,
		map[string]any{"name": "post-user", "role": "user", "quota_nano_usd": int64(1e12)}, &created)

	// 创建两把 Key
	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "notify-pool", "name": "k1", "secret": "bearer:sec-1"}, nil)
	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "notify-pool", "name": "k2", "secret": "bearer:sec-2"}, nil)

	// 2.1 测试 /rest/ 下 POST 请求（BodyStream 依 CORE-07 只尝试 1 次）
	req, err := http.NewRequest(http.MethodPost, dataURL+"/rest/notify/post", strings.NewReader(`{"msg":"critical"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /rest/notify/post: %v", err)
	}
	resp.Body.Close()

	if got := postAttempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt for /rest/ POST, got %d", got)
	}

	// 2.2 对比测试 /rest-idem/ 下 POST 请求（BodyBuffered 允许重试，将尝试多次）
	reqIdem, err := http.NewRequest(http.MethodPost, dataURL+"/rest-idem/notify/idem", strings.NewReader(`{"msg":"critical"}`))
	if err != nil {
		t.Fatal(err)
	}
	reqIdem.Header.Set("Authorization", "Bearer "+created.Token)
	reqIdem.Header.Set("Content-Type", "application/json")

	respIdem, err := http.DefaultClient.Do(reqIdem)
	if err != nil {
		t.Fatalf("POST /rest-idem/notify/idem: %v", err)
	}
	respIdem.Body.Close()

	if got := idemAttempts.Load(); got <= 1 {
		t.Fatalf("expected > 1 attempts for /rest-idem/ POST, got %d", got)
	}
}

// 3. 测试上游 403 错误（验证客户端收到 403 且 Key 状态保持 ready，不被隔离，并有限频 Warn）。
func TestE2E_REST_Upstream403_KeyNotQuarantined(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden_scope","message":"insufficient permissions"}}`)
	}))
	t.Cleanup(upstream.Close)

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:     "forbidden-pool",
			Protocol: "rest",
			BaseURL:  upstream.URL,
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "forbidden-svc",
			Pool:    "forbidden-pool",
			Price:   map[string]string{"requests": "0.01"},
			Reserve: map[string]int64{"requests": 1},
		})
		c.Normalize()
	})

	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data

	var created struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", adminURL+"/clients", g.adminToken,
		map[string]any{"name": "forbidden-user", "role": "user", "quota_nano_usd": int64(1e12)}, &created)

	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "forbidden-pool", "name": "k-403", "secret": "bearer:sec-403-token"}, nil)

	req, err := http.NewRequest(http.MethodGet, dataURL+"/rest/forbidden-svc/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /rest/forbidden-svc/data: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "forbidden_scope") {
		t.Errorf("body = %s, want to contain 'forbidden_scope'", string(body))
	}

	// 验证 Key 状态保持 ready，不被隔离
	var listRes struct {
		Keys []keyViewItem `json:"keys"`
	}
	doJSON(t, "GET", adminURL+"/keys", g.adminToken, nil, &listRes)
	var foundKey *keyViewItem
	for i := range listRes.Keys {
		if listRes.Keys[i].Pool == "forbidden-pool" {
			foundKey = &listRes.Keys[i]
			break
		}
	}
	if foundKey == nil {
		t.Fatal("key for forbidden-pool not found")
	}
	if foundKey.State != "ready" {
		t.Errorf("key state = %q, want ready", foundKey.State)
	}
	if foundKey.CooldownSeconds != 0 {
		t.Errorf("key cooldown = %d, want 0", foundKey.CooldownSeconds)
	}

	// 验证产生 403 限频 Warn
	stderrOutput := g.stderr.String()
	if !strings.Contains(stderrOutput, "rest upstream returned 403; key not quarantined") {
		t.Errorf("stderr does not contain 403 warn, got:\n%s", stderrOutput)
	}
}

// 4. 测试上游 401 错误（验证 Key 状态变为 quarantined 被隔离）。
func TestE2E_REST_Upstream401_KeyQuarantined(t *testing.T) {
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Done()
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_api_key"}`)
	}))
	t.Cleanup(upstream.Close)

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:     "authfail-pool",
			Protocol: "rest",
			BaseURL:  upstream.URL,
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "authfail-svc",
			Pool:    "authfail-pool",
			Price:   map[string]string{"requests": "0.01"},
			Reserve: map[string]int64{"requests": 1},
		})
		c.Normalize()
	})

	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data

	var created struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", adminURL+"/clients", g.adminToken,
		map[string]any{"name": "authfail-user", "role": "user", "quota_nano_usd": int64(1e12)}, &created)

	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "authfail-pool", "name": "k-401", "secret": "bearer:sec-401-token"}, nil)

	// 并发发送 2 次请求，使两笔请求都在完成前到达上游，触发连续 2 次 OutcomeAuthFailed (ST-06)
	var clientWg sync.WaitGroup
	clientWg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer clientWg.Done()
			req, _ := http.NewRequest(http.MethodGet, dataURL+"/rest/authfail-svc/data", nil)
			req.Header.Set("Authorization", "Bearer "+created.Token)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}

	// 等待两个请求都到达上游
	arrived.Wait()
	close(release) // 释放上游返回 401
	clientWg.Wait()

	// 轮询检查 Key 状态变为 quarantined (ST-06)
	deadline := time.Now().Add(3 * time.Second)
	var quarantined bool
	for {
		var listRes struct {
			Keys []keyViewItem `json:"keys"`
		}
		doJSON(t, "GET", adminURL+"/keys", g.adminToken, nil, &listRes)
		for _, k := range listRes.Keys {
			if k.Pool == "authfail-pool" {
				if k.State == "quarantined" {
					quarantined = true
					break
				}
			}
		}
		if quarantined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("key for authfail-pool was not quarantined within deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 5. 测试 query Key 网络中断时错误信息中无 query 凭据泄露。
func TestE2E_REST_QueryKey_NetworkError_NoCredentialLeak(t *testing.T) {
	const secretQueryToken = "super-secret-query-token-never-leak-999"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 劫持并直接关闭底层 TCP 连接，造成上游网络中断
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack not supported", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(upstream.Close)

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:     "neterr-pool",
			Protocol: "rest",
			BaseURL:  upstream.URL,
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "neterr-svc",
			Pool:    "neterr-pool",
			Price:   map[string]string{"requests": "0.01"},
			Reserve: map[string]int64{"requests": 1},
		})
		c.Normalize()
	})

	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data

	var created struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", adminURL+"/clients", g.adminToken,
		map[string]any{"name": "neterr-user", "role": "user", "quota_nano_usd": int64(1e12)}, &created)

	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "neterr-pool", "name": "k-neterr", "secret": "query:api_key:" + secretQueryToken}, nil)

	req, err := http.NewRequest(http.MethodGet, dataURL+"/rest/neterr-svc/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /rest/neterr-svc/ping: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// 客户端响应中绝不得包含凭据明文
	if strings.Contains(bodyStr, secretQueryToken) {
		t.Fatalf("credential leak in client response body: %s", bodyStr)
	}

	// 网关日志 stderr 中也绝不得包含凭据明文（AGENTS.md §3 禁止 1）
	stderrOutput := g.stderr.String()
	if strings.Contains(stderrOutput, secretQueryToken) {
		t.Fatalf("credential leak in gateway stderr logs:\n%s", stderrOutput)
	}
}
