package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	_ "modernc.org/sqlite"
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

// 6. 测试 /rest-idem/ 下 POST/PATCH 两把 Key 重试成功用例（第 1 把 Key 返回 500，调度器切到第 2 把 Key 成功 200），
// 断言两次出站请求体逐字节一致，且最终 usage_events 仅记录 1 次。
func TestE2E_REST_Idem_PostPatch_RetrySuccessAndBilling(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			var mu sync.Mutex
			var capturedBodies [][]byte
			var capturedKeys []string

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read body err", http.StatusInternalServerError)
					return
				}
				mu.Lock()
				capturedBodies = append(capturedBodies, body)
				capturedKeys = append(capturedKeys, r.Header.Get("Authorization"))
				attempt := len(capturedBodies)
				mu.Unlock()

				if attempt == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":"temporary_server_error"}`)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"recovered_ok"}`)
			}))
			defer upstream.Close()

			poolName := strings.ToLower(method) + "-idem-pool"
			modelName := strings.ToLower(method) + "-idem-svc"

			g := startRESTGateway(t, func(c *config.Config) {
				c.Pools = append(c.Pools, config.PoolConfig{
					Name:        poolName,
					Protocol:    "rest",
					BaseURL:     upstream.URL,
					MaxAttempts: 3,
				})
				c.Models = append(c.Models, config.ModelConfig{
					Name:    modelName,
					Pool:    poolName,
					Price:   map[string]string{"requests": "0.01"},
					Reserve: map[string]int64{"requests": 1},
				})
				c.Normalize()
			})

			adminURL := "http://" + g.admin + "/admin/api"
			dataURL := "http://" + g.data

			var created struct {
				Client struct {
					ID int64 `json:"id"`
				} `json:"client"`
				Token string `json:"token"`
			}
			doJSON(t, "POST", adminURL+"/clients", g.adminToken,
				map[string]any{"name": "idem-user-" + strings.ToLower(method), "role": "user", "quota_nano_usd": int64(1e12)}, &created)

			doJSON(t, "POST", adminURL+"/keys", g.adminToken,
				map[string]any{"pool": poolName, "name": "k1", "secret": "bearer:first-key-500"}, nil)
			doJSON(t, "POST", adminURL+"/keys", g.adminToken,
				map[string]any{"pool": poolName, "name": "k2", "secret": "bearer:second-key-200"}, nil)

			reqPayload := `{"action":"sync_state","data":"sample_payload_12345"}`
			req, err := http.NewRequest(method, dataURL+"/rest-idem/"+modelName+"/action/run", strings.NewReader(reqPayload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+created.Token)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s /rest-idem/%s/action/run: %v", method, modelName, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				respBody, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected status 200, got %d, body: %s", resp.StatusCode, string(respBody))
			}

			respBytes, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(respBytes), "recovered_ok") {
				t.Errorf("response body = %s, want to contain 'recovered_ok'", string(respBytes))
			}

			mu.Lock()
			totalAttempts := len(capturedBodies)
			mu.Unlock()

			if totalAttempts != 2 {
				t.Fatalf("expected 2 attempts, got %d", totalAttempts)
			}

			// 断言两把不同的 Key 参与了调度切换
			if capturedKeys[0] == capturedKeys[1] {
				t.Errorf("expected different keys on retry, got both %q", capturedKeys[0])
			}

			// 断言两次出站请求体逐字节一致，且与客户端请求体完全一致
			if !bytes.Equal(capturedBodies[0], capturedBodies[1]) {
				t.Errorf("outbound request bodies differ between attempts: %q vs %q", capturedBodies[0], capturedBodies[1])
			}
			if !bytes.Equal(capturedBodies[0], []byte(reqPayload)) {
				t.Errorf("outbound body = %q, want %q", capturedBodies[0], reqPayload)
			}

			// 断言最终 usage_events 仅记录 1 次
			// 1. 轮询管理接口 /admin/api/usage
			deadline := time.Now().Add(3 * time.Second)
			var usageItems []struct {
				ClientID int64            `json:"client_id"`
				Requests int64            `json:"requests"`
				Usage    map[string]int64 `json:"usage"`
			}
			for {
				var view struct {
					Items []struct {
						ClientID int64            `json:"client_id"`
						Requests int64            `json:"requests"`
						Usage    map[string]int64 `json:"usage"`
					} `json:"items"`
				}
				doJSON(t, "GET", fmt.Sprintf("%s/usage?client_id=%d", adminURL, created.Client.ID), g.adminToken, nil, &view)
				if len(view.Items) > 0 {
					usageItems = view.Items
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("usage query returned 0 items within deadline")
				}
				time.Sleep(20 * time.Millisecond)
			}

			if len(usageItems) != 1 {
				t.Fatalf("expected 1 usage item, got %d", len(usageItems))
			}
			if usageItems[0].Requests != 1 {
				t.Errorf("usage requests = %d, want 1", usageItems[0].Requests)
			}
			if usageItems[0].Usage["requests"] != 1 {
				t.Errorf("usage[requests] = %d, want 1", usageItems[0].Usage["requests"])
			}

			// 2. 停机并从底层 SQLite 表 usage_events 进一步断言仅有 1 行记录
			g.cancel()
			select {
			case err := <-g.done:
				if err != nil {
					t.Fatalf("gateway run failed: %v", err)
				}
				g.done <- nil
			case <-time.After(10 * time.Second):
				t.Fatal("gateway did not stop")
			}

			sdb, err := sql.Open("sqlite", filepath.Join(g.cfg.Server.DataDir, "gateway.db"))
			if err != nil {
				t.Fatalf("open sqlite db: %v", err)
			}
			defer sdb.Close()

			var rowCount int
			if err := sdb.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM usage_events WHERE client_id = ?", created.Client.ID).Scan(&rowCount); err != nil {
				t.Fatalf("SELECT COUNT(*) FROM usage_events: %v", err)
			}
			if rowCount != 1 {
				t.Fatalf("usage_events row count = %d, want exactly 1", rowCount)
			}
		})
	}
}

// 7. 测试 /rest-idem/ 非 POST/PATCH（如 PUT/GET/DELETE/HEAD）返回 404（RS-02）。
func TestE2E_REST_Idem_NonPostPatch_Returns404(t *testing.T) {
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:     "idem-check-pool",
			Protocol: "rest",
			BaseURL:  "http://127.0.0.1:1",
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "idem-check",
			Pool:    "idem-check-pool",
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
		map[string]any{"name": "idem-404-user", "role": "user", "quota_nano_usd": int64(1e12)}, &created)

	unsupportedMethods := []string{
		http.MethodGet,
		http.MethodPut,
		http.MethodDelete,
		http.MethodHead,
	}

	for _, method := range unsupportedMethods {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, dataURL+"/rest-idem/idem-check/resource", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+created.Token)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s /rest-idem/idem-check/resource: %v", method, err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s /rest-idem/ status = %d, want 404 Not Found", method, resp.StatusCode)
			}
		})
	}
}

// 8. 测试 GET 遇 429 换 Key 重试成功用例。
func TestE2E_REST_GET_RetryOn429(t *testing.T) {
	var mu sync.Mutex
	var capturedAuths []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedAuths = append(capturedAuths, r.Header.Get("Authorization"))
		attempt := len(capturedAuths)
		mu.Unlock()

		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate_limited_try_later"}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"recovered_from_429"}`)
	}))
	defer upstream.Close()

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:        "retry429-pool",
			Protocol:    "rest",
			BaseURL:     upstream.URL,
			MaxAttempts: 3,
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "retry429-svc",
			Pool:    "retry429-pool",
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
		map[string]any{"name": "user-429", "role": "user", "quota_nano_usd": int64(1e12)}, &created)

	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "retry429-pool", "name": "k1", "secret": "bearer:key-429-first"}, nil)
	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "retry429-pool", "name": "k2", "secret": "bearer:key-429-second"}, nil)

	req, err := http.NewRequest(http.MethodGet, dataURL+"/rest/retry429-svc/items", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /rest/retry429-svc/items: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body: %s", resp.StatusCode, string(respBody))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "recovered_from_429") {
		t.Errorf("body = %s, want to contain 'recovered_from_429'", string(body))
	}

	mu.Lock()
	attempts := len(capturedAuths)
	auth1 := capturedAuths[0]
	auth2 := capturedAuths[1]
	mu.Unlock()

	if attempts != 2 {
		t.Fatalf("expected 2 attempts upstream, got %d", attempts)
	}
	if auth1 == auth2 {
		t.Errorf("expected different keys on retry, got both %q", auth1)
	}
	keySet := map[string]bool{auth1: true, auth2: true}
	if !keySet["Bearer key-429-first"] || !keySet["Bearer key-429-second"] {
		t.Errorf("expected keys Bearer key-429-first and Bearer key-429-second, got %v", keySet)
	}
}

// 9. 测试白名单响应头透传断言（Link、Retry-After、RateLimit-Reset 等，且 Set-Cookie 严禁透传，RS-15）。
func TestE2E_REST_ResponseHeaders_WhitelistForwarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<https://api.example.com/items?page=2>; rel="next"`)
		w.Header().Set("Retry-After", "120")
		w.Header().Set("RateLimit-Reset", "1710000000")
		w.Header().Set("RateLimit-Limit", "5000")
		w.Header().Set("RateLimit-Remaining", "4999")
		w.Header().Set("RateLimit-Policy", "5000;w=3600")
		w.Header().Set("X-RateLimit-Reset", "1710000000")
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.Header().Set("Content-Disposition", `attachment; filename="export.json"`)
		w.Header().Set("ETag", `"v1.2.3"`)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Set-Cookie", "secret_session=leak_forbidden")

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer upstream.Close()

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{
			Name:     "header-pool",
			Protocol: "rest",
			BaseURL:  upstream.URL,
		})
		c.Models = append(c.Models, config.ModelConfig{
			Name:    "header-svc",
			Pool:    "header-pool",
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
		map[string]any{"name": "hdr-user", "role": "user", "quota_nano_usd": int64(1e12)}, &created)

	doJSON(t, "POST", adminURL+"/keys", g.adminToken,
		map[string]any{"pool": "header-pool", "name": "k-hdr", "secret": "bearer:key-hdr-sec"}, nil)

	req, err := http.NewRequest(http.MethodGet, dataURL+"/rest/header-svc/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /rest/header-svc/list: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// 白名单响应头透传验证 (RS-15)
	if got := resp.Header.Get("Link"); got != `<https://api.example.com/items?page=2>; rel="next"` {
		t.Errorf("Link = %q, want %q", got, `<https://api.example.com/items?page=2>; rel="next"`)
	}
	if got := resp.Header.Get("Retry-After"); got != "120" {
		t.Errorf("Retry-After = %q, want 120", got)
	}
	if got := resp.Header.Get("RateLimit-Reset"); got != "1710000000" {
		t.Errorf("RateLimit-Reset = %q, want 1710000000", got)
	}
	if got := resp.Header.Get("RateLimit-Limit"); got != "5000" {
		t.Errorf("RateLimit-Limit = %q, want 5000", got)
	}
	if got := resp.Header.Get("RateLimit-Remaining"); got != "4999" {
		t.Errorf("RateLimit-Remaining = %q, want 4999", got)
	}
	if got := resp.Header.Get("RateLimit-Policy"); got != "5000;w=3600" {
		t.Errorf("RateLimit-Policy = %q, want 5000;w=3600", got)
	}
	if got := resp.Header.Get("X-RateLimit-Reset"); got != "1710000000" {
		t.Errorf("X-RateLimit-Reset = %q, want 1710000000", got)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "42" {
		t.Errorf("X-RateLimit-Remaining = %q, want 42", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="export.json"` {
		t.Errorf("Content-Disposition = %q, want attachment", got)
	}
	if got := resp.Header.Get("ETag"); got != `"v1.2.3"` {
		t.Errorf("ETag = %q, want \"v1.2.3\"", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want public, max-age=3600", got)
	}

	// 严禁透传的敏感头（Set-Cookie 必须被丢弃）
	if got := resp.Header.Get("Set-Cookie"); got != "" {
		t.Errorf("Set-Cookie must be stripped, got: %q", got)
	}
}

// T54：rest 接口声明经 MCP 目录调用（RS-40 ~ RS-44、CAT-01 ~ CAT-08、TL-12）。
func TestE2E_REST_DeclaredTools_ViaMCPCatalog(t *testing.T) {
	type hit struct{ method, path, query, body, auth string }
	var mu sync.Mutex
	var hits []hit
	var flaky atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) // 测试上游：读取失败时 body 为空，由断言发现
		mu.Lock()
		hits = append(hits, hit{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/flaky":
			if flaky.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"busy"}`)
				return
			}
			_, _ = io.WriteString(w, `{"answer":"yes"}`)
		case "/once":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"down"}`)
		case "/err":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":"state is required"}`)
		default:
			_, _ = io.WriteString(w, `{"item":"ok"}`)
		}
	}))
	t.Cleanup(upstream.Close)
	hitCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(hits)
	}

	obj := func(props map[string]any, required ...any) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	g := startRESTGateway(t, func(c *config.Config) {
		c.MCP.ToolMode = config.ToolModeCatalog
		c.Pools = append(c.Pools, config.PoolConfig{Name: "tool-pool", Protocol: "rest", BaseURL: upstream.URL, MaxAttempts: 3})
		c.Models = append(c.Models, config.ModelConfig{
			Name: "toolsvc", Pool: "tool-pool",
			Price: map[string]string{"requests": "0.01"}, Reserve: map[string]int64{"requests": 1},
			Tools: []config.ToolConfig{
				{Name: "svc_get", Description: "Get one item by id from the inventory.", Method: "GET", Path: "/items/{id}",
					InputSchema: obj(map[string]any{"id": map[string]any{"type": "string"}}, "id")},
				{Name: "svc_judge", Description: "Judge a statement; retried on failure.", Method: "POST", Path: "/flaky", Idempotent: true},
				{Name: "svc_send", Description: "Send a notification once.", Method: "POST", Path: "/once"},
				{Name: "svc_err", Description: "Always rejected by upstream.", Method: "POST", Path: "/err", Idempotent: true},
			},
		})
		c.Normalize()
	})
	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data

	newClient := func(name string, allowed ...string) string {
		var created struct {
			Token string `json:"token"`
		}
		if code := doJSON(t, "POST", adminURL+"/clients", g.adminToken, map[string]any{
			"name": name, "role": "user", "quota_nano_usd": int64(1e12), "allowed_models": allowed}, &created); code != http.StatusCreated {
			t.Fatalf("create client %s: %d", name, code)
		}
		return created.Token
	}
	token := newClient("tool-user", "toolsvc")
	otherToken := newClient("other-user", "base-dummy-model")
	for i, k := range []string{"bearer:tool-secret-one", "bearer:tool-secret-two"} {
		if code := doJSON(t, "POST", adminURL+"/keys", g.adminToken,
			map[string]any{"pool": "tool-pool", "name": fmt.Sprintf("k%d", i+1), "secret": k}, nil); code != http.StatusCreated {
			t.Fatalf("create key: %d", code)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mc := &mcp.Client{URL: dataURL + "/mcp", Token: token, HTTP: http.DefaultClient}

	// CAT-03：目录模式只列出三个目录工具
	tools, err := mc.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "gateway_call_tool,gateway_describe_tool,gateway_search_tools" {
		t.Fatalf("tools/list = %v", names)
	}

	// CAT-04：搜索；CAT-05：查看
	res, err := mc.CallTool(ctx, mcp.CatalogSearchTool, json.RawMessage(`{"query":"inventory"}`))
	if err != nil || res.IsError || !strings.Contains(res.Content[0].Text, "svc_get") || strings.Contains(res.Content[0].Text, "svc_send") {
		t.Fatalf("search = %+v, %v", res, err)
	}
	res, err = mc.CallTool(ctx, mcp.CatalogDescribeTool, json.RawMessage(`{"name":"svc_send"}`))
	if err != nil || res.IsError || !strings.Contains(string(res.StructuredContent), `"readOnlyHint":false`) {
		t.Fatalf("describe = %+v, %v", res, err)
	}

	// RS-42、TL-12：占位符段内的 / 转义为 %2F，其余参数进查询串
	res, err = mc.CallTool(ctx, mcp.CatalogCallTool, json.RawMessage(`{"name":"svc_get","arguments":{"id":"a/b c","q":"x y","tag":["1","2"]}}`))
	if err != nil || res.IsError || res.Content[0].Text != `{"item":"ok"}` || !strings.Contains(string(res.StructuredContent), `"item"`) {
		t.Fatalf("call svc_get = %+v, %v", res, err)
	}
	mu.Lock()
	h := hits[len(hits)-1]
	mu.Unlock()
	if h.method != "GET" || h.path != "/items/a%2Fb%20c" || h.query != "q=x+y&tag=1&tag=2" || h.body != "" || !strings.HasPrefix(h.auth, "Bearer tool-secret-") {
		t.Errorf("upstream saw %+v", h)
	}

	// RS-19：idempotent 的 POST 遇 503 换 Key 重试，两次请求体相同
	before := hitCount()
	res, err = mc.CallTool(ctx, mcp.CatalogCallTool, json.RawMessage(`{"name":"svc_judge","arguments":{"state":"s","n":12345678901234567890}}`))
	if err != nil || res.IsError || !strings.Contains(res.Content[0].Text, "yes") {
		t.Fatalf("call svc_judge = %+v, %v", res, err)
	}
	mu.Lock()
	judged := append([]hit(nil), hits[before:]...)
	mu.Unlock()
	if len(judged) != 2 || judged[0].body != judged[1].body || judged[0].body != `{"n":12345678901234567890,"state":"s"}` || judged[0].auth == judged[1].auth {
		t.Errorf("idempotent attempts = %+v", judged)
	}

	// CORE-07：非 idempotent 的 POST 只尝试一次
	before = hitCount()
	res, err = mc.CallTool(ctx, mcp.CatalogCallTool, json.RawMessage(`{"name":"svc_send","arguments":{"msg":"hello-arg"}}`))
	if err != nil || !res.IsError {
		t.Fatalf("call svc_send = %+v, %v", res, err)
	}
	if n := hitCount() - before; n != 1 {
		t.Errorf("svc_send attempts = %d, want 1", n)
	}

	// RS-43：上游 4xx 的错误体出现在工具结果中
	res, err = mc.CallTool(ctx, mcp.CatalogCallTool, json.RawMessage(`{"name":"svc_err"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].Text, "HTTP 422") || !strings.Contains(res.Content[0].Text, "state is required") {
		t.Fatalf("call svc_err = %+v, %v", res, err)
	}

	// CAT-07：隐藏的工具可以用原名直接调用；命令行 gateway call、gateway tools 可用
	res, err = mc.CallTool(ctx, "svc_get", json.RawMessage(`{"id":"7"}`))
	if err != nil || res.IsError {
		t.Fatalf("direct svc_get = %+v, %v", res, err)
	}
	var out, errb bytes.Buffer
	cliEnv := func(k string) string {
		return map[string]string{"UAIG_URL": dataURL, "UAIG_TOKEN": token}[k]
	}
	if code := realMainIO([]string{"call", "svc_get", "--args", `{"id":"cli"}`}, strings.NewReader(""), &out, &errb, cliEnv); code != 0 || !strings.Contains(out.String(), `"item"`) {
		t.Fatalf("gateway call: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := realMainIO([]string{"tools"}, strings.NewReader(""), &out, &errb, cliEnv); code != 0 || strings.Count(out.String(), "svc_") != 4 {
		t.Fatalf("gateway tools: code=%d out=%q", code, out.String())
	}

	// TL-03：白名单外的客户端看不到也调不了这些接口
	other := &mcp.Client{URL: dataURL + "/mcp", Token: otherToken, HTTP: http.DefaultClient}
	res, err = other.CallTool(ctx, mcp.CatalogSearchTool, json.RawMessage(`{}`))
	if err != nil || strings.Contains(res.Content[0].Text, "svc_") {
		t.Fatalf("other search = %+v, %v", res, err)
	}
	res, err = other.CallTool(ctx, mcp.CatalogCallTool, json.RawMessage(`{"name":"svc_get","arguments":{"id":"1"}}`))
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].Text, "unknown tool") {
		t.Fatalf("other call = %+v, %v", res, err)
	}

	// 计费：成功 4 次（svc_get ×3 含命令行、svc_judge ×1）；搜索、查看与失败调用不计费
	deadline := time.Now().Add(3 * time.Second)
	for {
		var usage struct {
			Items []struct {
				Requests int64 `json:"requests"`
			} `json:"items"`
		}
		doJSON(t, "GET", adminURL+"/usage?client=tool-user", g.adminToken, nil, &usage)
		var n int64
		for _, it := range usage.Items {
			n += it.Requests
		}
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("billed requests = %d (%+v), want 4", n, usage.Items)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if log := g.stderr.String(); strings.Contains(log, "tool-secret-") || strings.Contains(log, "hello-arg") || strings.Contains(log, "inventory") {
		t.Errorf("log leaks secrets or arguments:\n%s", log)
	}
}

// CFG-18：非 rest 池上的服务声明接口 → 拒绝启动
func TestRestToolDefsRejectNonRestPool(t *testing.T) {
	cfg := &config.Config{
		Pools:  []config.PoolConfig{{Name: "tv", Protocol: "tavily"}},
		Models: []config.ModelConfig{{Name: "s", Pool: "tv", Tools: []config.ToolConfig{{Name: "x", Method: "GET", Path: "/"}}}},
	}
	if _, err := restToolDefs(cfg); err == nil || !strings.Contains(err.Error(), "does not support declared tools") {
		t.Fatalf("restToolDefs = %v", err)
	}
}

// CFG-19：置顶的工具必须存在
func TestNewRegistryRejectsUnknownPinnedTool(t *testing.T) {
	cfg := &config.Config{MCP: config.MCPConfig{PinnedTools: []string{"no_such_tool"}}}
	if _, err := newRegistry(cfg, nil); err == nil || !strings.Contains(err.Error(), "pinned_tools") {
		t.Fatalf("newRegistry = %v", err)
	}
	cfg.MCP.PinnedTools = []string{"tavily_search"}
	if _, err := newRegistry(cfg, nil); err != nil {
		t.Fatalf("valid pin rejected: %v", err)
	}
}
