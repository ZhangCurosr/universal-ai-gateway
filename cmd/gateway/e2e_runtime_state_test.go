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

// T68：正常关闭后用同一数据目录重启，冷却中的 Key 仍在冷却，重启前创建的异步任务仍固定到创建它的 Key。
func TestE2E_RuntimeStateSurvivesRestart(t *testing.T) {
	var mu sync.Mutex
	owner := map[string]string{}
	var polls []string
	seq := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/limit":
			if key == "Bearer rk1-secret-aaaa" {
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":"quota"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/task":
			seq++
			id := "job-" + string(rune('a'+seq))
			owner[id] = key
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"`+id+`"}}`)
		case strings.HasPrefix(r.URL.Path, "/task/"):
			id := strings.TrimPrefix(r.URL.Path, "/task/")
			polls = append(polls, key)
			if owner[id] != key {
				_, _ = io.WriteString(w, `{"code":-1,"msg":"task not found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"state":"done"}}`)
		}
	}))
	defer upstream.Close()

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "rs", Protocol: "rest", BaseURL: upstream.URL, MaxAttempts: 3, PinTTL: 48 * time.Hour,
			Cooldown: config.CooldownConfig{Max: 24 * time.Hour}})
		c.Models = append(c.Models, config.ModelConfig{Name: "rs-api", Pool: "rs", Price: map[string]string{"requests": "0"},
			API: &config.APIConfig{
				Errors: []config.APIErrorRule{{When: config.APIWhen{JSON: "code", NotIn: []any{0}}, Outcome: "client_error", Status: 404}},
				Endpoints: []config.APIEndpoint{
					{Match: "POST /task", TaskID: "data.id"},
					{Match: "GET /task/{id}", PinnedBy: "id"},
				}}})
		c.Normalize()
	})
	admin := "http://" + g.admin + "/admin/api"
	var cl struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", admin+"/clients", g.adminToken, map[string]any{"name": "rs", "role": "user", "quota_nano_usd": int64(1e12)}, &cl)
	for _, k := range []string{"rk1-secret-aaaa", "rk2-secret-bbbb", "rk3-secret-cccc"} {
		doJSON(t, "POST", admin+"/keys", g.adminToken, map[string]any{"pool": "rs", "name": k[:3], "secret": k}, nil)
	}
	call := func(data, method, path string) (int, string) {
		t.Helper()
		var rd io.Reader
		if method == http.MethodPost {
			rd = strings.NewReader(`{}`)
		}
		req, _ := http.NewRequest(method, "http://"+data+path, rd)
		req.Header.Set("Authorization", "Bearer "+cl.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// rk1 进入 1 小时冷却
	for i := 0; i < 4; i++ {
		if st, body := call(g.data, http.MethodGet, "/rest/rs-api/limit"); st != 200 {
			t.Fatalf("limit %d: %d %s", i, st, body)
		}
	}
	// 3 个异步任务，分别固定到各自的 Key
	var ids []string
	for i := 0; i < 3; i++ {
		st, body := call(g.data, http.MethodPost, "/rest-idem/rs-api/task")
		if st != 200 {
			t.Fatalf("submit: %d %s", st, body)
		}
		ids = append(ids, body[strings.Index(body, `"id":"`)+6:strings.Index(body, `"}}`)])
	}

	// 正常关闭（RT-12：关闭时写最后一次快照），再用同一配置与数据目录启动
	g.cancel()
	if err := <-g.done; err != nil {
		t.Fatalf("first run: %v", err)
	}
	g.done <- nil // 交还给 startRESTGateway 的清理函数
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
	admin = "http://" + addrs[1] + "/admin/api"

	var keys struct {
		Keys []keyViewItem `json:"keys"`
	}
	doJSON(t, "GET", admin+"/keys", g.adminToken, nil, &keys)
	for _, k := range keys.Keys {
		if k.Name == "rk1" && k.CooldownSeconds < 3500 {
			t.Fatalf("rk1 cooldown after restart = %ds, want about 3600 (%+v)", k.CooldownSeconds, keys.Keys)
		}
		if k.Name == "rk2" && k.CooldownSeconds != 0 {
			t.Fatalf("rk2 must not be cooling: %+v", k)
		}
	}
	mu.Lock()
	polls = nil
	mu.Unlock()
	for round := 0; round < 3; round++ {
		for _, id := range ids {
			if st, body := call(addrs[0], http.MethodGet, "/rest/rs-api/task/"+id); st != 200 || !strings.Contains(body, "done") {
				t.Fatalf("poll %s after restart: %d %s", id, st, body)
			}
		}
	}
	if !strings.Contains(g.stderr.String(), `"msg":"runtime state restored"`) || strings.Contains(g.stderr.String(), "rk1-secret") {
		t.Fatalf("restore log missing or leaks a key:\n%s", g.stderr.String())
	}
	for _, line := range strings.Split(g.stderr.String(), "\n") {
		if !strings.Contains(line, "runtime state") {
			continue // 请求日志中的路径本来就含任务 ID（已有行为），这里只检查 T68 写的日志（RT-14）
		}
		for _, id := range ids {
			if strings.Contains(line, id) {
				t.Fatalf("runtime state log contains a task id: %s", line)
			}
		}
	}
}
