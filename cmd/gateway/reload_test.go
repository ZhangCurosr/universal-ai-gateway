package main

import (
	"bytes"
	"context"
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
)

// ---------------------------------------------------------------- 单元：配置比较（HR-12）

func baseCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server: { data_dir: ./data, master_key_file: ./secrets/master.key }
pools:
  - { name: p1, protocol: rest, base_url: "https://a.example" }
  - { name: p2, protocol: rest, base_url: "https://b.example" }
models:
  - { name: s1, pool: p1, price: { requests: "0.01" } }
  - { name: s2, pool: p2, price: { requests: "0.01" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDiffConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		mode    string
		check   func(reloadChanges) bool
		explain string
	}{
		{"identical", func(*config.Config) {}, reloadModeNone, func(c reloadChanges) bool { return c.Pools.empty() && c.Services.empty() }, ""},
		{"service price", func(c *config.Config) { c.Models[0].Price["requests"] = "0.02" }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Services.Changed, ",") == "s1" }, "services.changed=[s1]"},
		{"service added and removed", func(c *config.Config) {
			c.Models[1].Name = "s3"
		}, reloadModeHot, func(c reloadChanges) bool {
			return strings.Join(c.Services.Added, ",") == "s3" && strings.Join(c.Services.Removed, ",") == "s2"
		}, "s3 added, s2 removed"},
		{"pool base_url", func(c *config.Config) { c.Pools[1].BaseURL = "https://c.example" }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Pools.Changed, ",") == "p2" }, "pools.changed=[p2]"},
		{"pool removed", func(c *config.Config) { c.Pools = c.Pools[:1]; c.Models = c.Models[:1] }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Pools.Removed, ",") == "p2" }, "pools.removed=[p2]"},
		{"mcp", func(c *config.Config) { c.MCP.ToolMode = config.ToolModeCatalog }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Settings, ",") == "mcp.tool_mode" }, "settings=[mcp.tool_mode]"},
		{"translation", func(c *config.Config) { c.Translation.UnknownFields = "drop" }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Settings, ",") == "translation.unknown_fields" }, ""},
		{"log level", func(c *config.Config) { c.Server.LogLevel = "debug" }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Settings, ",") == "server.log_level" }, ""},
		{"trusted proxies", func(c *config.Config) { c.Server.TrustedProxies = []string{"10.0.0.0/8"} }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Settings, ",") == "server.trusted_proxies" }, ""},
		{"admin hosts", func(c *config.Config) { c.Server.AdminHosts = []string{"gw.lan"} }, reloadModeHot,
			func(c reloadChanges) bool { return strings.Join(c.Settings, ",") == "server.admin_hosts" }, ""},
		{"data listen", func(c *config.Config) { c.Server.DataListen = "127.0.0.1:9999" }, reloadModeRestart,
			func(c reloadChanges) bool { return strings.Join(c.RestartFields, ",") == "server.data_listen" }, ""},
		{"data dir", func(c *config.Config) { c.Server.DataDir = "./other" }, reloadModeRestart,
			func(c reloadChanges) bool { return strings.Join(c.RestartFields, ",") == "server.data_dir" }, ""},
		{"limits", func(c *config.Config) { c.Limits.MaxInFlightRequests = 7 }, reloadModeRestart,
			func(c reloadChanges) bool {
				return strings.Join(c.RestartFields, ",") == "limits.max_in_flight_requests"
			}, ""},
		{"ledger", func(c *config.Config) { c.Ledger.FlushInterval = time.Second }, reloadModeRestart,
			func(c reloadChanges) bool { return strings.Join(c.RestartFields, ",") == "ledger.flush_interval" }, ""},
		{"audit", func(c *config.Config) { c.Audit.RetentionDays = 3 }, reloadModeRestart,
			func(c reloadChanges) bool { return strings.Join(c.RestartFields, ",") == "audit.retention_days" }, ""},
		{"restart wins over hot", func(c *config.Config) { c.Limits.PreAuthIPRPM = 1; c.Models[0].Price["requests"] = "0.5" }, reloadModeRestart,
			func(c reloadChanges) bool { return len(c.RestartFields) == 1 && len(c.Services.Changed) == 1 }, "both listed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldCfg, newCfg := baseCfg(t), baseCfg(t)
			tt.mutate(newCfg)
			mode, ch := diffConfig(oldCfg, newCfg)
			if mode != tt.mode || !tt.check(ch) {
				b, _ := json.Marshal(ch)
				t.Fatalf("mode = %s, changes = %s; want %s %s", mode, b, tt.mode, tt.explain)
			}
			b, _ := json.Marshal(ch)
			if strings.Contains(string(b), "null") {
				t.Errorf("changes contain null slices: %s", b)
			}
		})
	}
}

func TestReloadLogCapAndFinish(t *testing.T) {
	l := &reloadLog{}
	var first int64
	for i := 0; i < reloadRecordCap+5; i++ {
		id := l.add(reloadRecord{Status: reloadStatusApplied})
		if i == reloadRecordCap+4 {
			first = id
		}
	}
	l.finish(first, reloadStatusRolledBack, "boom")
	recs := l.view().(map[string]any)["records"].([]reloadRecord)
	if len(recs) != reloadRecordCap || recs[0].ID != first || recs[0].Status != reloadStatusRolledBack || recs[0].Error != "boom" {
		t.Fatalf("records = %d, newest = %+v", len(recs), recs[0])
	}
}

// HR-10：同一时间只有一次重载
func TestReloadConcurrentConflict(t *testing.T) {
	rl := &configReloader{path: "x", logger: slog.New(slog.NewTextHandler(io.Discard, nil)), records: &reloadLog{}}
	rl.mu.Lock()
	if status, _ := rl.Reload(context.Background(), 1, false); status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	rl.mu.Unlock()
	rl.restarted.Store(true)
	if status, _ := rl.Reload(context.Background(), 1, false); status != http.StatusConflict {
		t.Fatalf("after restart started: status = %d, want 409", status)
	}
	if status, _ := (&configReloader{}).Reload(context.Background(), 1, false); status != http.StatusNotFound {
		t.Fatalf("no config path: status = %d, want 404", status)
	}
}

// ---------------------------------------------------------------- 端到端

type reloadGW struct {
	t                 *testing.T
	dir, path         string
	data, admin       string // 当前实际地址
	adminToken, token string
	stderr            *syncBuffer
	ready             chan [2]string
	done              chan error
	cancel            context.CancelFunc
}

type gwSpec struct {
	dataListen, adminListen string
	dataDir, masterKey      string // 相对 dir
	logLevel                string
	extra                   string // 追加的顶层 YAML（limits 等）
	services                string // models 列表项
	pools                   string // pools 列表项
}

func (g *reloadGW) render(s gwSpec) string {
	def := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	return fmt.Sprintf(`server:
  data_listen: %q
  admin_listen: %q
  data_dir: %q
  master_key_file: %q
  shutdown_grace: 5s
  log_level: %s
ledger:
  flush_interval: 20ms
%s
pools:
%s
models:
%s
`, def(s.dataListen, "127.0.0.1:0"), def(s.adminListen, "127.0.0.1:0"),
		filepath.ToSlash(filepath.Join(g.dir, def(s.dataDir, "data"))),
		filepath.ToSlash(filepath.Join(g.dir, def(s.masterKey, "secrets/master.key"))),
		def(s.logLevel, "info"), s.extra, s.pools, s.services)
}

func (g *reloadGW) write(s gwSpec) {
	g.t.Helper()
	if err := os.WriteFile(g.path, []byte(g.render(s)), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// startReloadGateway 用 serveLoop（与 gateway serve 相同）启动，配置文件路径用于重载。
func startReloadGateway(t *testing.T, s gwSpec) *reloadGW {
	t.Helper()
	dir := t.TempDir()
	g := &reloadGW{t: t, dir: dir, path: filepath.Join(dir, "gateway.yaml"), stderr: &syncBuffer{}, ready: make(chan [2]string, 4), done: make(chan error, 1)}
	g.write(s)
	cfg, err := config.Load(g.path)
	if err != nil {
		t.Fatal(err)
	}
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	go func() {
		g.done <- serveLoop(ctx, cfg, runOptions{Stderr: g.stderr, ConfigPath: g.path,
			OnListening: func(d, a net.Addr) { g.ready <- [2]string{d.String(), a.String()} }})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-g.done:
		case <-time.After(15 * time.Second):
			t.Error("gateway did not stop")
		}
	})
	g.awaitListening()
	const prefix = "BOOTSTRAP ADMIN TOKEN (shown once): "
	for _, line := range strings.Split(g.stderr.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			g.adminToken = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	if g.adminToken == "" {
		t.Fatalf("no bootstrap token:\n%s", g.stderr.String())
	}
	return g
}

func (g *reloadGW) awaitListening() {
	g.t.Helper()
	select {
	case a := <-g.ready:
		g.data, g.admin = a[0], a[1]
	case err := <-g.done:
		g.t.Fatalf("gateway exited: %v\n%s", err, g.stderr.String())
	case <-time.After(15 * time.Second):
		g.t.Fatalf("gateway not listening\n%s", g.stderr.String())
	}
}

func (g *reloadGW) adminURL() string { return "http://" + g.admin + "/admin/api" }

func (g *reloadGW) reload(dryRun bool) (int, map[string]any) {
	g.t.Helper()
	var out map[string]any
	code := doJSON(g.t, "POST", g.adminURL()+"/reload", g.adminToken, map[string]any{"dry_run": dryRun}, &out)
	return code, out
}

// waitRecord 等待重载记录 id 离开 restarting，返回最终状态（进程内重启完成的同步信号是新实例的 OnListening）。
func (g *reloadGW) waitRecord(id float64) map[string]any {
	g.t.Helper()
	g.awaitListening()
	var out struct {
		Records []map[string]any `json:"records"`
	}
	doJSON(g.t, "GET", g.adminURL()+"/reload", g.adminToken, nil, &out)
	for _, r := range out.Records {
		if r["id"] == id {
			return r
		}
	}
	g.t.Fatalf("record %v not found: %+v", id, out.Records)
	return nil
}

func (g *reloadGW) createClient(name string) string {
	g.t.Helper()
	var created struct {
		Token string `json:"token"`
	}
	if code := doJSON(g.t, "POST", g.adminURL()+"/clients", g.adminToken, map[string]any{"name": name, "role": "user", "quota_nano_usd": int64(1e12)}, &created); code != http.StatusCreated {
		g.t.Fatalf("create client: %d", code)
	}
	return created.Token
}

func (g *reloadGW) addKey(pool, name string) {
	g.t.Helper()
	if code := doJSON(g.t, "POST", g.adminURL()+"/keys", g.adminToken, map[string]any{"pool": pool, "name": name, "secret": "bearer:reload-test-secret-" + name}, nil); code != http.StatusCreated {
		g.t.Fatalf("create key: %d", code)
	}
}

func (g *reloadGW) get(token, path string) (int, string) {
	g.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+g.data+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (g *reloadGW) spent(client string) int64 {
	g.t.Helper()
	var usage struct {
		Items []struct {
			Cost int64 `json:"cost_nano_usd"`
		} `json:"items"`
	}
	doJSON(g.t, "GET", g.adminURL()+"/usage?client="+client, g.adminToken, nil, &usage)
	var n int64
	for _, it := range usage.Items {
		n += it.Cost
	}
	return n
}

func (g *reloadGW) waitSpent(client string, want int64) {
	g.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.spent(client) != want {
		if time.Now().After(deadline) {
			g.t.Fatalf("spent = %d, want %d", g.spent(client), want)
		}
		time.Sleep(20 * time.Millisecond) // 等待账本刷盘（异步），轮询有上限
	}
}

func restUpstream(t *testing.T, release <-chan struct{}, entered chan<- struct{}) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			entered <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"path":"`+r.URL.Path+`"}`)
	}))
	t.Cleanup(up.Close)
	return up.URL
}

func poolYAML(name, url string) string {
	return fmt.Sprintf("  - { name: %s, protocol: rest, base_url: %q }\n", name, url)
}

func svcYAML(name, pool, price string) string {
	return fmt.Sprintf("  - { name: %s, pool: %s, price: { requests: %q }, reserve: { requests: 1 } }\n", name, pool, price)
}

// HR-13、HR-14：热重载新增服务、改价格；在途请求按旧价格结算；Key 状态保留；日志级别生效
func TestE2E_Reload_Hot(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	url := restUpstream(t, release, entered)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0.01")}
	g := startReloadGateway(t, spec)
	g.addKey("p1", "k1")
	token := g.createClient("u")

	if code, _ := g.get(token, "/rest/s1/x"); code != 200 {
		t.Fatalf("s1 before reload = %d", code)
	}
	if code, _ := g.get(token, "/rest/s2/x"); code != 400 {
		t.Fatalf("s2 before reload = %d, want 400 unknown model", code)
	}

	// 慢请求在重载前进入上游
	slowDone := make(chan int, 1)
	go func() { code, _ := g.get(token, "/rest/s1/slow"); slowDone <- code }()
	<-entered

	spec.services = svcYAML("s1", "p1", "0.02") + svcYAML("s2", "p1", "0.01")
	spec.logLevel = "debug"
	g.write(spec)
	code, out := g.reload(true) // dry-run 不应用
	if code != 200 || out["mode"] != reloadModeHot || out["applied"] != false {
		t.Fatalf("dry-run = %d %+v", code, out)
	}
	if code, _ := g.get(token, "/rest/s2/x"); code != 400 {
		t.Fatalf("dry-run applied the change: s2 = %d", code)
	}
	code, out = g.reload(false)
	if code != 200 || out["mode"] != reloadModeHot || out["applied"] != true {
		t.Fatalf("reload = %d %+v", code, out)
	}
	changes := out["changes"].(map[string]any)
	if fmt.Sprint(changes["services"].(map[string]any)["added"]) != "[s2]" || fmt.Sprint(changes["settings"]) != "[server.log_level]" {
		t.Errorf("changes = %+v", changes)
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("log level not switched to debug")
	}
	if code, _ := g.get(token, "/rest/s2/x"); code != 200 {
		t.Fatalf("s2 after reload = %d", code)
	}

	close(release)
	if code := <-slowDone; code != 200 {
		t.Fatalf("in-flight request across reload = %d", code)
	}
	// 0.01（s1 重载前）+ 0.01（慢请求，旧价格）+ 0.01（s2）= 30_000_000；新价格的 s1 请求再加 0.02
	g.waitSpent("u", 30_000_000)
	if code, _ := g.get(token, "/rest/s1/x"); code != 200 {
		t.Fatal("s1 after reload failed")
	}
	g.waitSpent("u", 50_000_000)

	// 配置错误 → 400；构造失败（未知协议）→ 422；两种情况下旧配置继续生效
	if err := os.WriteFile(g.path, []byte("pools: [oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := g.reload(false); code != http.StatusBadRequest {
		t.Fatalf("bad yaml = %d", code)
	}
	bad := spec
	bad.pools = poolYAML("p1", url) + "  - { name: px, protocol: nosuch, base_url: \"http://127.0.0.1:1\" }\n"
	bad.services = spec.services + svcYAML("sx", "px", "0")
	g.write(bad)
	if code, _ := g.reload(false); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown protocol = %d", code)
	}
	if code, _ := g.get(token, "/rest/s2/x"); code != 200 {
		t.Fatal("previous configuration lost after failed reload")
	}
	var recs struct {
		Records []map[string]any `json:"records"`
	}
	doJSON(t, "GET", g.adminURL()+"/reload", g.adminToken, nil, &recs)
	if len(recs.Records) != 3 || recs.Records[0]["status"] != reloadStatusFailed || recs.Records[2]["status"] != reloadStatusApplied {
		t.Errorf("records = %+v", recs.Records)
	}
	if strings.Contains(g.stderr.String(), "reload-test-secret") || strings.Contains(g.stderr.String(), url) {
		t.Error("log leaks secrets or config content")
	}
}

// HR-19、SCH-20：删除池后 Key 显示 orphaned，恢复后回归调度
func TestE2E_Reload_OrphanedKeys(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url) + poolYAML("p2", url), services: svcYAML("s1", "p1", "0") + svcYAML("s2", "p2", "0")}
	g := startReloadGateway(t, spec)
	g.addKey("p2", "k2")
	token := g.createClient("u")

	keyState := func() (string, string) {
		var out struct {
			Keys []struct {
				State       string `json:"state"`
				StoredState string `json:"stored_state"`
			} `json:"keys"`
		}
		doJSON(t, "GET", g.adminURL()+"/keys", g.adminToken, nil, &out)
		return out.Keys[0].State, out.Keys[0].StoredState
	}
	removed := spec
	removed.pools, removed.services = poolYAML("p1", url), svcYAML("s1", "p1", "0")
	g.write(removed)
	code, out := g.reload(false)
	if code != 200 || len(out["warnings"].([]any)) != 1 {
		t.Fatalf("remove pool = %d %+v", code, out)
	}
	if st, stored := keyState(); st != "orphaned" || stored != "ready" {
		t.Fatalf("key state = %s / %s", st, stored)
	}
	g.write(spec)
	if code, _ := g.reload(false); code != 200 {
		t.Fatal("restore pool failed")
	}
	if st, _ := keyState(); st != "ready" {
		t.Fatalf("key state after restore = %s", st)
	}
	if code, _ := g.get(token, "/rest/s2/x"); code != 200 {
		t.Fatalf("restored pool not scheduled: %d", code)
	}
}

// HR-15：修改 limits → 进程内重启；地址不变时沿用套接字，期间的请求不失败；计费不丢
func TestE2E_Reload_RestartKeepsServing(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0.01")}
	g := startReloadGateway(t, spec)
	g.addKey("p1", "k1")
	token := g.createClient("u")
	if code, _ := g.get(token, "/rest/s1/before"); code != 200 {
		t.Fatal("request before restart failed")
	}
	dataAddr, adminAddr := g.data, g.admin

	spec.extra = "limits:\n  max_in_flight_requests: 64\n"
	g.write(spec)
	if code, out := g.reload(true); code != 200 || out["mode"] != reloadModeRestart || out["applied"] != false {
		t.Fatalf("dry-run = %d %+v", code, out)
	}

	var failures atomic.Int32
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // 重启期间持续发请求
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if code, body := g.get(token, "/rest/s1/during"); code != 200 {
				failures.Add(1)
				t.Logf("request during restart: %d %s", code, body)
			}
		}
	}()
	code, out := g.reload(false)
	if code != http.StatusAccepted || out["mode"] != reloadModeRestart {
		t.Fatalf("reload = %d %+v", code, out)
	}
	rec := g.waitRecord(out["record_id"].(float64))
	close(stop)
	wg.Wait()
	if rec["status"] != reloadStatusApplied {
		t.Fatalf("record = %+v", rec)
	}
	if g.data != dataAddr || g.admin != adminAddr {
		t.Errorf("addresses changed: %s %s -> %s %s", dataAddr, adminAddr, g.data, g.admin)
	}
	if n := failures.Load(); n != 0 {
		t.Errorf("%d requests failed during restart", n)
	}
	if code, _ := g.get(token, "/rest/s1/after"); code != 200 {
		t.Fatal("request after restart failed")
	}
	// 计费：重启前后的请求都已落库（不知道期间请求的确切数量，只检查至少 2 次）
	deadline := time.Now().Add(5 * time.Second)
	for g.spent("u") < 20_000_000 {
		if time.Now().After(deadline) {
			t.Fatalf("spent = %d", g.spent("u"))
		}
		time.Sleep(20 * time.Millisecond) // 等待异步刷盘，有上限
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// §2.3：修改监听地址；新地址被占用时预检失败、旧实例不受影响
func TestE2E_Reload_ListenAddress(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0")}
	g := startReloadGateway(t, spec)
	g.addKey("p1", "k1")
	token := g.createClient("u")
	oldData := g.data

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	spec.dataListen = busy.Addr().String()
	g.write(spec)
	if code, out := g.reload(false); code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out), "not available") {
		t.Fatalf("busy address = %d %+v", code, out)
	}
	if code, _ := g.get(token, "/rest/s1/x"); code != 200 {
		t.Fatal("old instance affected by failed preflight")
	}

	spec.dataListen = freeAddr(t)
	g.write(spec)
	code, out := g.reload(false)
	if code != http.StatusAccepted {
		t.Fatalf("reload = %d %+v", code, out)
	}
	if rec := g.waitRecord(out["record_id"].(float64)); rec["status"] != reloadStatusApplied {
		t.Fatalf("record = %+v", rec)
	}
	if g.data != spec.dataListen {
		t.Fatalf("data address = %s, want %s", g.data, spec.dataListen)
	}
	if code, _ := g.get(token, "/rest/s1/x"); code != 200 {
		t.Fatal("new address does not serve")
	}
	if c, err := net.DialTimeout("tcp", oldData, time.Second); err == nil {
		c.Close()
		t.Error("old data address still accepts connections")
	}
}

// HR-16、待定 6：新实例启动失败 → 用旧配置回滚
func TestE2E_Reload_RollbackOnStartupFailure(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0")}
	g := startReloadGateway(t, spec)
	g.addKey("p1", "k1")
	token := g.createClient("u")

	// 新 data_dir 中放一个不是 SQLite 的 gateway.db：预检（目录与锁）通过，打开数据库时失败
	if err := os.MkdirAll(filepath.Join(g.dir, "data2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.dir, "data2", "gateway.db"), bytes.Repeat([]byte("not sqlite "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.dataDir = "data2"
	spec.masterKey = "secrets/master.key"
	g.write(spec)
	code, out := g.reload(false)
	if code != http.StatusAccepted {
		t.Fatalf("reload = %d %+v", code, out)
	}
	rec := g.waitRecord(out["record_id"].(float64))
	if rec["status"] != reloadStatusRolledBack || rec["error"] == "" {
		t.Fatalf("record = %+v", rec)
	}
	if code, _ := g.get(token, "/rest/s1/x"); code != 200 {
		t.Fatal("rolled-back instance does not serve with the old data")
	}
}

// 待定 5：master_key_file 变化的三种情况
func TestE2E_Reload_MasterKeyRules(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0")}
	g := startReloadGateway(t, spec)

	cur, err := os.ReadFile(filepath.Join(g.dir, "secrets", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	other := append([]byte(nil), cur...)
	other[0] ^= 0xff
	if err := os.WriteFile(filepath.Join(g.dir, "secrets", "other.key"), other, 0o600); err != nil {
		t.Fatal(err)
	}

	spec.masterKey = "secrets/missing.key"
	g.write(spec)
	if code, _ := g.reload(true); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing key = %d", code)
	}
	spec.masterKey = "secrets/other.key"
	g.write(spec)
	if code, out := g.reload(true); code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out), "different key") {
		t.Fatalf("different key = %d %+v", code, out)
	}
	spec.dataDir = "data-new" // 同时换数据库：允许
	g.write(spec)
	code, out := g.reload(true)
	if code != 200 || !strings.Contains(fmt.Sprint(out["warnings"]), "bootstrap admin token") {
		t.Fatalf("different key + new data_dir = %d %+v", code, out)
	}

	if err := os.WriteFile(filepath.Join(g.dir, "secrets", "moved.key"), cur, 0o600); err != nil {
		t.Fatal(err)
	}
	spec.dataDir, spec.masterKey = "", "secrets/moved.key" // 同样的密钥换了文件：允许并真正重启
	g.write(spec)
	code, out = g.reload(false)
	if code != http.StatusAccepted {
		t.Fatalf("moved key = %d %+v", code, out)
	}
	if rec := g.waitRecord(out["record_id"].(float64)); rec["status"] != reloadStatusApplied {
		t.Fatalf("record = %+v", rec)
	}
	var health map[string]any
	if code := doJSON(t, "GET", g.adminURL()+"/health", g.adminToken, nil, &health); code != 200 {
		t.Fatalf("admin token invalid after restart with moved key: %d", code)
	}
}

// ADM-18：非管理员不能重载；HR-02：gateway reload 命令
func TestE2E_Reload_CLIAndAuth(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0")}
	g := startReloadGateway(t, spec)
	user := g.createClient("u")
	if code := doJSON(t, "POST", g.adminURL()+"/reload", user, map[string]any{}, nil); code != http.StatusForbidden {
		t.Fatalf("user reload = %d, want 403", code)
	}

	env := func(k string) string {
		return map[string]string{"UAIG_ADMIN_URL": "http://" + g.admin, "UAIG_ADMIN_TOKEN": g.adminToken}[k]
	}
	cli := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := realMainIO(append([]string{"reload"}, args...), strings.NewReader(""), &out, &errb, env)
		return code, out.String(), errb.String()
	}
	if code, out, _ := cli(); code != 0 || !strings.Contains(out, "mode: none") {
		t.Fatalf("no change: %d %q", code, out)
	}
	spec.services += svcYAML("s2", "p1", "0")
	g.write(spec)
	if code, out, _ := cli("--dry-run"); code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, "services added: s2") {
		t.Fatalf("dry-run: %d %q", code, out)
	}
	if code, out, _ := cli(); code != 0 || !strings.Contains(out, "mode: hot (applied)") {
		t.Fatalf("hot: %d %q", code, out)
	}
	spec.extra = "limits:\n  preauth_ip_rpm: 1000\n"
	g.write(spec)
	code, out, errOut := cli("--json")
	if code != 0 || !strings.Contains(out, `"final_status": "applied"`) {
		t.Fatalf("restart via cli: %d %q %q", code, out, errOut)
	}
	g.awaitListening()
	if err := os.WriteFile(g.path, []byte("::: not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(); code != 1 || !strings.Contains(errOut, "reload failed") {
		t.Fatalf("bad config: %d %q", code, errOut)
	}
	if code, _, errOut := realMainIOEnv([]string{"reload"}, func(string) string { return "" }); code != 2 || !strings.Contains(errOut, "UAIG_ADMIN_TOKEN") {
		t.Fatalf("missing token: %d %q", code, errOut)
	}
	if strings.Contains(out+errOut, g.adminToken) {
		t.Error("cli output leaks the admin token")
	}
}

func realMainIOEnv(args []string, env func(string) string) (int, string, string) {
	var out, errb bytes.Buffer
	code := realMainIO(args, strings.NewReader(""), &out, &errb, env)
	return code, out.String(), errb.String()
}

// ADM-20：工具列表随热重载更新
func TestE2E_Reload_ToolsEndpoint(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0")}
	g := startReloadGateway(t, spec)
	tools := func() string {
		var out map[string]any
		if code := doJSON(t, "GET", g.adminURL()+"/tools", g.adminToken, nil, &out); code != 200 {
			t.Fatalf("GET tools = %d", code)
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	if s := tools(); strings.Contains(s, "s1_get") || !strings.Contains(s, `"tool_mode":"auto"`) {
		t.Fatalf("before = %s", s)
	}
	spec.services = "  - name: s1\n    pool: p1\n    price: { requests: \"0\" }\n    tools:\n      - { name: s1_get, description: \"Get it.\", method: GET, path: /x }\n"
	g.write(spec)
	if code, out := g.reload(false); code != 200 {
		t.Fatalf("reload = %d %+v", code, out)
	}
	if s := tools(); !strings.Contains(s, `"name":"s1_get"`) || !strings.Contains(s, `"service":"s1"`) || !strings.Contains(s, `"read_only":true`) {
		t.Fatalf("after = %s", s)
	}
}
