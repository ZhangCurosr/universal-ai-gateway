package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// T66：声明式 API 能力经完整网关：异步任务固定到创建它的 Key、业务额度错误换 Key 并冷却、
// 按声明计费、未定价单位自动补 0（DA-13）。
func TestE2E_REST_DeclaredAPI(t *testing.T) {
	var mu sync.Mutex
	taskOwner := map[string]string{} // 任务 ID → 创建它的 Key
	exhausted := map[string]bool{}   // 日额度已用完的 Key
	var submits, polls []string      // 每次请求所用的 Key
	seq := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/extract/task":
			submits = append(submits, key)
			if exhausted[key] {
				_, _ = io.WriteString(w, `{"code":-60018,"msg":"daily quota"}`)
				return
			}
			seq++
			id := "task-" + string(rune('0'+seq))
			taskOwner[id] = key
			_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"`+id+`"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/extract/task/"):
			polls = append(polls, key)
			id := strings.TrimPrefix(r.URL.Path, "/api/v4/extract/task/")
			if taskOwner[id] != key {
				_, _ = io.WriteString(w, `{"code":-60012,"msg":"task not found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"state":"done"}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer upstream.Close()

	notIn := []any{0}
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "mu-rest", Protocol: "rest", BaseURL: upstream.URL, MaxAttempts: 3, Cooldown: config.CooldownConfig{Max: 24 * time.Hour}})
		c.Models = append(c.Models, config.ModelConfig{
			Name: "mu-api", Pool: "mu-rest",
			Price:   map[string]string{"pages": "0.01", "requests": "0"},
			Reserve: map[string]int64{"pages": 600},
			API: &config.APIConfig{
				Errors: []config.APIErrorRule{
					{When: config.APIWhen{JSON: "code", In: []any{-60018}}, Outcome: "rate_limited", CooldownUntil: &config.APICooldown{Time: "00:01", TZ: "+08:00"}},
					{When: config.APIWhen{JSON: "code", In: []any{-60012}}, Outcome: "client_error", Status: 404},
					{When: config.APIWhen{JSON: "code", NotIn: notIn}, Outcome: "client_error"},
				},
				Endpoints: []config.APIEndpoint{
					{Match: "POST /api/v4/extract/task", TaskID: "data.task_id", Usage: map[string]config.APIUsage{"pages": {RequestQuery: "pages", Default: ptrInt64(600)}}},
					{Match: "GET /api/v4/extract/task/{task}", PinnedBy: "task", Usage: map[string]config.APIUsage{}},
				},
			},
		})
		// 另一个 rest 服务只按次计费：价目表里没有 pages，装配时补 0（DA-13）
		c.Models = append(c.Models, config.ModelConfig{Name: "plain-api", Pool: "mu-rest", Price: map[string]string{"requests": "0.001"}})
		c.Normalize()
	})
	admin := "http://" + g.admin + "/admin/api"
	data := "http://" + g.data
	var cl struct {
		Client struct {
			ID int64 `json:"id"`
		} `json:"client"`
		Token string `json:"token"`
	}
	doJSON(t, "POST", admin+"/clients", g.adminToken, map[string]any{"name": "decl", "role": "user", "quota_nano_usd": int64(1e12)}, &cl)
	for _, k := range []string{"k1-secret-aaaa", "k2-secret-bbbb", "k3-secret-cccc"} {
		doJSON(t, "POST", admin+"/keys", g.adminToken, map[string]any{"pool": "mu-rest", "name": k[:2], "secret": "bearer:" + k}, nil)
	}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, data+path, rd)
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
	taskID := func(body string) string {
		var v struct {
			Data struct {
				TaskID string `json:"task_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &v) // 解析失败时返回空串，由调用方断言
		return v.Data.TaskID
	}

	// 1. 提交 3 个任务，每个都只用创建它的 Key 查询（3 把 Key 里随机落到别的 Key 会得到 -60012）
	var ids []string
	for i := 0; i < 3; i++ {
		st, body := call("POST", "/rest-idem/mu-api/api/v4/extract/task?pages=13", `{"url":"https://example.com/a.pdf"}`)
		if st != 200 || taskID(body) == "" {
			t.Fatalf("submit %d: %d %s", i, st, body)
		}
		ids = append(ids, taskID(body))
	}
	for round := 0; round < 4; round++ {
		for _, id := range ids {
			if st, body := call("GET", "/rest/mu-api/api/v4/extract/task/"+id, ""); st != 200 || !strings.Contains(body, "done") {
				t.Fatalf("poll %s: %d %s", id, st, body)
			}
		}
	}
	if st, body := call("GET", "/rest/mu-api/api/v4/extract/task/never-created", ""); st != 404 {
		t.Fatalf("unknown task: %d %s", st, body)
	}

	// 2. 两把 Key 的日额度用完：提交换到第三把 Key 并成功；用完的 Key 被冷却
	mu.Lock()
	ks := []string{"Bearer k1-secret-aaaa", "Bearer k2-secret-bbbb"}
	exhausted[ks[0]], exhausted[ks[1]] = true, true
	submits = nil
	mu.Unlock()
	for i := 0; i < 2; i++ {
		if st, body := call("POST", "/rest-idem/mu-api/api/v4/extract/task?pages=2", `{"url":"https://example.com/b.pdf"}`); st != 200 {
			t.Fatalf("submit with exhausted keys %d: %d %s", i, st, body)
		}
	}
	mu.Lock()
	tried := append([]string(nil), submits...)
	mu.Unlock()
	if last := tried[len(tried)-1]; last != "Bearer k3-secret-cccc" || len(tried) > 4 {
		t.Fatalf("submits after quota = %d attempts, last %q", len(tried), last)
	}
	var keys struct {
		Keys []keyViewItem `json:"keys"`
	}
	doJSON(t, "GET", admin+"/keys", g.adminToken, nil, &keys)
	cooling := 0
	for _, k := range keys.Keys {
		if k.Pool == "mu-rest" && k.CooldownSeconds > 3600 {
			cooling++
		}
	}
	if cooling != 2 {
		t.Fatalf("keys cooling until the quota reset = %d, want 2: %+v", cooling, keys.Keys)
	}

	// 3. 计费：3×13 + 2×2 页；查询不计费；没有声明的服务按次计费且启动成功（补 0）
	if st, body := call("GET", "/rest/plain-api/anything", ""); st != 200 {
		t.Fatalf("plain-api status %d %s", st, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var u struct {
			Items []struct {
				Model string           `json:"model"`
				Usage map[string]int64 `json:"usage"`
			} `json:"items"`
		}
		doJSON(t, "GET", admin+"/usage", g.adminToken, nil, &u)
		pages, plain := int64(-1), int64(-1)
		for _, it := range u.Items {
			switch it.Model {
			case "mu-api":
				pages = it.Usage["pages"]
			case "plain-api":
				plain = it.Usage["requests"]
			}
		}
		if pages == 43 && plain == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("billed pages = %d (want 43), plain requests = %d (want 1): %+v", pages, plain, u.Items)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, s := range []string{"k1-secret-aaaa", "k2-secret-bbbb", "k3-secret-cccc"} {
		if strings.Contains(g.stderr.String(), s) {
			t.Fatal("gateway log leaks an upstream key")
		}
	}
}

func ptrInt64(v int64) *int64 { return &v }

// DA-01：api 只能声明在 rest 池的服务上。
func TestRestAPIDefsRejectNonRestPool(t *testing.T) {
	cfg := &config.Config{
		Pools:  []config.PoolConfig{{Name: "tv", Protocol: "tavily"}},
		Models: []config.ModelConfig{{Name: "s", Pool: "tv", API: &config.APIConfig{}}},
	}
	if _, err := restAPIDefs(cfg); err == nil || !strings.Contains(err.Error(), "does not support api declarations") {
		t.Fatalf("err = %v", err)
	}
}

func TestWithRestUnitPrices(t *testing.T) {
	cfg := &config.Config{
		Pools: []config.PoolConfig{{Name: "r", Protocol: "rest"}, {Name: "o", Protocol: "openai"}},
		Models: []config.ModelConfig{
			{Name: "a", Pool: "r", Price: map[string]string{"requests": "1", "pages": "0.01"}},
			{Name: "b", Pool: "r", Price: map[string]string{"requests": "1"}},
			{Name: "c", Pool: "o", Price: map[string]string{"input_tokens": "1"}},
		},
	}
	out := withRestUnitPrices(cfg, []string{"credits", "pages", "requests"})
	if out.Models[0].Price["pages"] != "0.01" || out.Models[0].Price["credits"] != "0" || out.Models[1].Price["pages"] != "0" {
		t.Fatalf("filled = %+v", out.Models)
	}
	if _, ok := out.Models[2].Price["pages"]; ok {
		t.Fatal("non-rest model must not be filled")
	}
	if _, ok := cfg.Models[1].Price["pages"]; ok {
		t.Fatal("original config modified")
	}
}

// T67：同步等待 flow 经完整网关：一次请求拿到结果包中的 full.md，按轮询中的页数计费；
// 提交成功后任务失败时不换 Key 重试，计费保留（FL-08、CORE-12）。
func TestE2E_REST_DeclaredFlow(t *testing.T) {
	var mu sync.Mutex
	submits := 0
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("out/full.md")
	_, _ = io.WriteString(w, "# Parsed\n")
	_ = zw.Close() // 写入内存，不会失败
	zipBytes := buf.Bytes()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/submit":
			submits++
			body, _ := io.ReadAll(r.Body)
			id := "ok"
			if strings.Contains(string(body), "broken") {
				id = "bad"
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"`+id+`"}}`)
		case r.URL.Path == "/task/ok":
			_, _ = io.WriteString(w, `{"code":0,"data":{"state":"done","pages":7,"zip":"http://`+r.Host+`/zip"}}`)
		case r.URL.Path == "/task/bad":
			_, _ = io.WriteString(w, `{"code":0,"data":{"state":"failed","pages":3,"err":"unsupported file"}}`)
		case r.URL.Path == "/zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBytes)
		}
	}))
	defer upstream.Close()

	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "flow-rest", Protocol: "rest", BaseURL: upstream.URL, MaxAttempts: 3})
		c.Models = append(c.Models, config.ModelConfig{
			Name: "flow-api", Pool: "flow-rest",
			Price: map[string]string{"pages": "0.01", "requests": "0"}, Reserve: map[string]int64{"pages": 100},
			API: &config.APIConfig{Endpoints: []config.APIEndpoint{{
				Match: "POST /parse",
				Usage: map[string]config.APIUsage{"pages": {JSON: "data.pages"}},
				Flow: &config.APIFlow{
					Submit: &config.APIFlowSubmit{Request: "POST /submit", TaskID: "data.task_id"},
					Poll:   &config.APIFlowPoll{Request: "GET /task/{task_id}", State: "data.state", Done: []string{"done"}, Failed: []string{"failed"}},
					Result: &config.APIFlowResult{URL: "data.zip", ZipEntry: "full.md", ContentType: "text/markdown; charset=utf-8"},
				},
			}}},
		})
		c.Normalize()
	})
	admin := "http://" + g.admin + "/admin/api"
	var cl struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", admin+"/clients", g.adminToken, map[string]any{"name": "flow", "role": "user", "quota_nano_usd": int64(1e12)}, &cl)
	for _, k := range []string{"fk1-secret-aaaa", "fk2-secret-bbbb"} {
		doJSON(t, "POST", admin+"/keys", g.adminToken, map[string]any{"pool": "flow-rest", "name": k[:3], "secret": k}, nil)
	}
	post := func(body string) (int, string, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+g.data+"/rest-idem/flow-api/parse", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+cl.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	if st, ct, body := post(`{"url":"https://example.com/a.pdf"}`); st != 200 || ct != "text/markdown; charset=utf-8" || body != "# Parsed\n" {
		t.Fatalf("sync parse: %d %q %q", st, ct, body)
	}
	if st, _, body := post(`{"url":"https://example.com/broken.pdf"}`); st != 400 || !strings.Contains(body, "unsupported file") {
		t.Fatalf("failed task: %d %q", st, body)
	}
	mu.Lock()
	n := submits
	mu.Unlock()
	if n != 2 {
		t.Fatalf("submits = %d, want 2 (a failed task must not be resubmitted on another key)", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var u struct {
			Items []struct {
				Model string           `json:"model"`
				Usage map[string]int64 `json:"usage"`
			} `json:"items"`
		}
		doJSON(t, "GET", admin+"/usage", g.adminToken, nil, &u)
		pages := int64(0)
		for _, it := range u.Items {
			if it.Model == "flow-api" {
				pages += it.Usage["pages"]
			}
		}
		if pages == 10 { // 7（完成）+ 3（失败前见到的页数，保留计费）
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("billed pages = %d, want 10", pages)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
