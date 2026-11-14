package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// T63：drain 策略下一把 Key 用满后主动换下一把，上游不会收到任何「额度用尽」的请求；
// 全部用满后返回 503 与 Retry-After；重启后用量从计费事件恢复（上限改为 4 后每把只能再用 1 次）。
func TestE2E_QuotaDrainAndRestart(t *testing.T) {
	var mu sync.Mutex
	perKey := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		perKey[r.Header.Get("Authorization")]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "qp", Protocol: "rest", BaseURL: upstream.URL, MaxAttempts: 3,
			Quota: &config.QuotaConfig{Unit: "requests", Limit: 3, Period: config.QuotaPeriodDay, Reset: &config.QuotaReset{TZ: "+08:00"}}})
		c.Models = append(c.Models, config.ModelConfig{Name: "qp-api", Pool: "qp", Price: map[string]string{"requests": "0"}})
		c.Normalize()
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	admin := "http://" + g.admin + "/admin/api"
	var cl struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", admin+"/clients", g.adminToken, map[string]any{"name": "q", "role": "user", "quota_nano_usd": int64(1e12)}, &cl)
	for _, k := range []string{"qk1-secret-aaaa", "qk2-secret-bbbb"} {
		doJSON(t, "POST", admin+"/keys", g.adminToken, map[string]any{"pool": "qp", "name": k[:3], "secret": k}, nil)
	}
	call := func(data string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+data+"/rest/qp-api/search", nil)
		req.Header.Set("Authorization", "Bearer "+cl.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	for i := 0; i < 6; i++ {
		if st, body, _ := call(g.data); st != 200 {
			t.Fatalf("call %d: %d %s", i, st, body)
		}
		mu.Lock()
		k1, k2 := perKey["Bearer qk1-secret-aaaa"], perKey["Bearer qk2-secret-bbbb"]
		mu.Unlock()
		if want1 := min(i+1, 3); k1 != want1 || k2 != i+1-want1 { // 先把 qk1 用满，再用 qk2
			t.Fatalf("after call %d: qk1=%d qk2=%d", i, k1, k2)
		}
	}
	st, body, h := call(g.data)
	if st != http.StatusServiceUnavailable || !strings.Contains(body, "2 quota exhausted") || h.Get("Retry-After") == "" {
		t.Fatalf("exhausted: %d %s retry-after %q", st, body, h.Get("Retry-After"))
	}
	var keys struct {
		Keys []keyViewItem `json:"keys"`
	}
	doJSON(t, "GET", admin+"/keys", g.adminToken, nil, &keys)
	for _, k := range keys.Keys {
		if k.Pool == "qp" && (k.State != "exhausted" || k.QuotaUsed != 3 || k.QuotaLimit != 3) {
			t.Fatalf("key view = %+v", k)
		}
	}

	// 正常关闭（计费事件刷盘），上限改为 4 后重启：用量从计费事件恢复为 3，每把只能再用 1 次
	g.cancel()
	if err := <-g.done; err != nil {
		t.Fatalf("first run: %v", err)
	}
	g.done <- nil // 交还给 startRESTGateway 的清理函数
	for i := range g.cfg.Pools {
		if g.cfg.Pools[i].Name == "qp" {
			g.cfg.Pools[i].Quota.Limit = 4
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan [2]string, 1)
	go func() {
		done <- run(ctx, g.cfg, runOptions{Stderr: g.stderr, OnListening: func(d, a net.Addr) { ready <- [2]string{d.String(), a.String()} }})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("restarted gateway did not stop")
		}
	})
	var addrs [2]string
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("restart failed: %v\n%s", err, g.stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("restart did not listen")
	}
	for i := 0; i < 2; i++ {
		if st, body, _ := call(addrs[0]); st != 200 {
			t.Fatalf("after restart call %d: %d %s", i, st, body)
		}
	}
	if st, body, _ := call(addrs[0]); st != http.StatusServiceUnavailable {
		t.Fatalf("usage was not restored: %d %s", st, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if perKey["Bearer qk1-secret-aaaa"] != 4 || perKey["Bearer qk2-secret-bbbb"] != 4 {
		t.Fatalf("upstream calls per key = %v", perKey)
	}
	if !strings.Contains(g.stderr.String(), `"msg":"quota usage restored"`) || strings.Contains(g.stderr.String(), "qk1-secret") {
		t.Fatalf("restore log missing or leaks a key:\n%s", g.stderr.String())
	}
}
