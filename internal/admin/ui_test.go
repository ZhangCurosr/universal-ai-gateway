package admin

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
)

// serverWith 用 newTestEnv 的依赖加上 mutate 的修改构造一个新的管理面。
func serverWith(t *testing.T, mutate func(*Deps)) (*testEnv, http.Handler) {
	t.Helper()
	env := newTestEnv(t)
	d := Deps{
		Config:    env.cfg,
		Store:     env.store,
		Keyring:   env.keyring,
		Index:     env.index,
		Reloader:  env.reloader,
		Ledger:    env.ledger,
		Scheduler: env.scheduler,
		Logger:    obs.Discard(),
		Version:   "1.0.0-test",
		Now:       func() time.Time { return *env.now },
	}
	if mutate != nil {
		mutate(&d)
	}
	return env, NewServer(d)
}

func uiGet(h http.Handler, method, target, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "127.0.0.1:50000"
	if host != "" {
		r.Host = host
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func embeddedNames(t *testing.T) []string {
	t.Helper()
	var names []string
	err := fs.WalkDir(uiFiles, "ui", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, strings.TrimPrefix(p, "ui/"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded ui: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded ui files")
	}
	return names
}

// UI-01、UI-03、UI-04：每个内嵌文件都能取到，Content-Type 与安全头正确。
func TestUIServesEmbeddedFiles(t *testing.T) {
	env := newTestEnv(t)
	for _, name := range embeddedNames(t) {
		w := uiGet(env.server, http.MethodGet, uiPrefix+name, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, w.Code)
		}
		if got, want := w.Header().Get("Content-Type"), uiContentTypes[path.Ext(name)]; got != want {
			t.Errorf("%s: Content-Type = %q, want %q", name, got, want)
		}
		wantCache := "no-cache"
		if name == "index.html" {
			wantCache = "no-store"
		}
		if got := w.Header().Get("Cache-Control"); got != wantCache {
			t.Errorf("%s: Cache-Control = %q, want %q", name, got, wantCache)
		}
		assertSecurityHeaders(t, name, w)
	}
}

func assertSecurityHeaders(t *testing.T, name string, w *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"X-Frame-Options":         "DENY",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", name, k, got, v)
		}
	}
}

func TestUIRoutes(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		name, method, target string
		wantStatus           int
	}{
		{"root redirects", http.MethodGet, "/admin/ui", http.StatusMovedPermanently},
		{"index", http.MethodGet, "/admin/ui/", http.StatusOK},
		{"head index", http.MethodHead, "/admin/ui/", http.StatusOK},
		{"missing file", http.MethodGet, "/admin/ui/nope.js", http.StatusNotFound},
		{"unknown extension", http.MethodGet, "/admin/ui/app.map", http.StatusNotFound},
		{"directory", http.MethodGet, "/admin/ui/pages/", http.StatusNotFound},
		{"post", http.MethodPost, "/admin/ui/", http.StatusMethodNotAllowed},
		{"delete file", http.MethodDelete, "/admin/ui/app.js", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := uiGet(env.server, tt.method, tt.target, "")
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			assertSecurityHeaders(t, tt.target, w)
		})
	}

	w := uiGet(env.server, http.MethodGet, "/admin/ui", "")
	if loc := w.Header().Get("Location"); loc != "/admin/ui/" {
		t.Errorf("redirect Location = %q", loc)
	}
	w = uiGet(env.server, http.MethodPost, "/admin/ui/", "")
	if allow := w.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q", allow)
	}
	w = uiGet(env.server, http.MethodHead, "/admin/ui/", "")
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d", w.Body.Len())
	}
}

// 路径穿越不能绕到其他处理器拿到数据，也不能读到内嵌目录以外的文件。
func TestUIPathTraversal(t *testing.T) {
	env := newTestEnv(t)
	for _, target := range []string{
		"/admin/ui/../api/clients",
		"/admin/ui/..%2fapi%2fclients",
		"/admin/ui/%2e%2e/api/clients",
		"/admin/ui/../../go.mod",
		"/admin/ui/./../ui.go",
	} {
		w := uiGet(env.server, http.MethodGet, target, "")
		if w.Code == http.StatusOK {
			t.Errorf("%s: status 200, body %q", target, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `"clients"`) || strings.Contains(w.Body.String(), "module ") {
			t.Errorf("%s: leaked data: %q", target, w.Body.String())
		}
	}
}

// UI-02：静态资源不需要 Token，也不计入 ADM-04 的失败次数；API 仍然要鉴权。
func TestUINoAuthAndNoLockout(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 30; i++ {
		if w := uiGet(env.server, http.MethodGet, "/admin/ui/app.js", ""); w.Code != http.StatusOK {
			t.Fatalf("static request %d: status %d", i, w.Code)
		}
	}
	if w := env.req(http.MethodGet, "/admin/api/health", nil, env.adminTok); w.Code != http.StatusOK {
		t.Fatalf("admin API after static requests: status %d (lockout triggered?)", w.Code)
	}
	if w := env.req(http.MethodGet, "/admin/api/clients", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("API without token: status %d, want 401", w.Code)
	}
}

var (
	inlineScript = regexp.MustCompile(`(?is)<script\b[^>]*>\s*[^<\s]`)
	styleAttr    = regexp.MustCompile(`(?i)\sstyle\s*=`)
	eventAttr    = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	styleTag     = regexp.MustCompile(`(?i)<style\b`)
)

// UI-05、UI-06、UI-09、UI-12：静态扫描内嵌资源。
func TestUIStaticRules(t *testing.T) {
	total := 0
	for _, name := range embeddedNames(t) {
		b, err := fs.ReadFile(uiFiles, "ui/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		total += len(b)
		src := string(b)
		switch path.Ext(name) {
		case ".html":
			if inlineScript.MatchString(src) {
				t.Errorf("%s: inline <script> content", name)
			}
			if styleAttr.MatchString(src) || styleTag.MatchString(src) {
				t.Errorf("%s: inline style", name)
			}
			if eventAttr.MatchString(src) {
				t.Errorf("%s: inline event handler attribute", name)
			}
		case ".js":
			for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "localStorage", "document.cookie", "setAttribute('style'", `setAttribute("style"`} {
				if strings.Contains(src, banned) {
					t.Errorf("%s: contains %q", name, banned)
				}
			}
			if strings.Contains(src, "http://") || strings.Contains(src, "https://") {
				// 只允许 SVG 命名空间这一个 URL（不发起网络请求）
				cleaned := strings.ReplaceAll(src, "http://www.w3.org/2000/svg", "")
				if strings.Contains(cleaned, "http://") || strings.Contains(cleaned, "https://") {
					t.Errorf("%s: references an absolute URL", name)
				}
			}
		case ".css":
			if strings.Contains(src, "@import") || strings.Contains(src, "url(http") {
				t.Errorf("%s: loads external resources", name)
			}
		}
	}
	if total > 300*1024 {
		t.Errorf("embedded ui is %d bytes, limit is 300 KiB (UI-06)", total)
	}
}

// UI-20、UI-21：Host 检查。
func TestHostCheck(t *testing.T) {
	_, h := serverWith(t, func(d *Deps) {
		d.HostCheck = true
		d.AdminHosts = []string{"Gw.Example.Internal", "[fe80::1]"}
	})
	tests := []struct {
		host string
		want int
	}{
		{"127.0.0.1:8081", http.StatusOK},
		{"localhost:8081", http.StatusOK},
		{"LOCALHOST", http.StatusOK},
		{"[::1]:8081", http.StatusOK},
		{"gw.example.internal:9000", http.StatusOK},
		{"[fe80::1]:8081", http.StatusOK},
		{"evil.example.com", http.StatusMisdirectedRequest},
		{"evil.example.com:8081", http.StatusMisdirectedRequest},
		{"127.0.0.1.evil.com", http.StatusMisdirectedRequest},
		{"", http.StatusMisdirectedRequest},
	}
	for _, tt := range tests {
		for _, target := range []string{"/admin/ui/", "/admin/metrics", "/admin/api/health"} {
			r := httptest.NewRequest(http.MethodGet, target, nil)
			r.RemoteAddr = "127.0.0.1:50000"
			r.Host = tt.host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			got := w.Code
			// 允许的 Host：health 无 Token 时 401，metrics 未配置时 404，都说明通过了 Host 检查
			if tt.want == http.StatusOK && got == http.StatusMisdirectedRequest {
				t.Errorf("host %q %s: rejected", tt.host, target)
			}
			if tt.want == http.StatusMisdirectedRequest {
				if got != http.StatusMisdirectedRequest {
					t.Errorf("host %q %s: status %d, want 421", tt.host, target, got)
					continue
				}
				var body struct {
					Error struct{ Code, Message string } `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "invalid_request" || body.Error.Message != "host not allowed" {
					t.Errorf("host %q: body %q", tt.host, w.Body.String())
				}
			}
		}
	}
}

// Host 检查拒绝的请求不计入 ADM-04 的失败次数。
func TestHostCheckDoesNotCountFailures(t *testing.T) {
	env, h := serverWith(t, func(d *Deps) { d.HostCheck = true })
	for i := 0; i < 30; i++ {
		uiGet(h, http.MethodGet, "/admin/api/clients", "evil.example.com")
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/api/health", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Host = "127.0.0.1:8081"
	r.Header.Set("Authorization", "Bearer "+env.adminTok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d after rejected hosts, want 200", w.Code)
	}
}

// 零值 Deps 不做 Host 检查（UI-21）：现有测试用 example.com 作 Host 也能通过。
func TestHostCheckOffByDefault(t *testing.T) {
	env := newTestEnv(t)
	if w := uiGet(env.server, http.MethodGet, "/admin/ui/", "evil.example.com"); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
}

// spec/09 §6.4：GET /admin/api/config。
func TestConfigEndpoint(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Pools[0].BaseURL = "https://secret-host.example.com/v1"
	env.cfg.Models[0].Price = map[string]string{"input_tokens": "3/1M"}

	if w := env.req(http.MethodGet, "/admin/api/config", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("without token: status %d, want 401", w.Code)
	}
	w := env.req(http.MethodGet, "/admin/api/config", nil, env.adminTok)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got ConfigView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantPools := []ConfigPool{{Name: "anthropic-pool", Protocol: "anthropic"}, {Name: "openai-pool", Protocol: "openai"}}
	wantModels := []ConfigModel{{Name: "claude-3-5-sonnet", Pool: "anthropic-pool"}, {Name: "gpt-4o", Pool: "openai-pool"}}
	if len(got.Pools) != 2 || got.Pools[0] != wantPools[0] || got.Pools[1] != wantPools[1] {
		t.Errorf("pools = %+v, want %+v", got.Pools, wantPools)
	}
	if len(got.Models) != 2 || got.Models[0] != wantModels[0] || got.Models[1] != wantModels[1] {
		t.Errorf("models = %+v, want %+v", got.Models, wantModels)
	}
	body := w.Body.String()
	for _, leak := range []string{"secret-host", "base_url", "price", "3/1M"} {
		if strings.Contains(body, leak) {
			t.Errorf("config response contains %q: %s", leak, body)
		}
	}
}

func TestConfigEndpointNilConfig(t *testing.T) {
	env, h := serverWith(t, func(d *Deps) { d.Config = nil })
	r := httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Authorization", "Bearer "+env.adminTok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"pools":[],"models":[]}` {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
}

// spec/09 §6.4：health 的附加字段。
func TestHealthExtraFields(t *testing.T) {
	env, h := serverWith(t, func(d *Deps) {
		d.LedgerQueue = func() (int, int) { return 3, 4096 }
		d.AuditDropped = func() int64 { return 12 }
	})
	r := httptest.NewRequest(http.MethodGet, "/admin/api/health", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Authorization", "Bearer "+env.adminTok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var got struct {
		Status      string `json:"status"`
		LedgerQueue *struct {
			Length   int `json:"length"`
			Capacity int `json:"capacity"`
		} `json:"ledger_queue"`
		AuditDropped *int64 `json:"audit_dropped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" || got.LedgerQueue == nil || got.LedgerQueue.Length != 3 || got.LedgerQueue.Capacity != 4096 || got.AuditDropped == nil || *got.AuditDropped != 12 {
		t.Fatalf("health = %s", w.Body.String())
	}

	// 没有注入时不输出
	w = env.req(http.MethodGet, "/admin/api/health", nil, env.adminTok)
	if strings.Contains(w.Body.String(), "ledger_queue") || strings.Contains(w.Body.String(), "audit_dropped") {
		t.Fatalf("health without gauges = %s", w.Body.String())
	}
}

// UI-30：孤立的 Key 有专门的显示与文案（中英文），不落入默认的「可用」。
func TestUIOrphanedKeyState(t *testing.T) {
	read := func(name string) string {
		b, err := fs.ReadFile(uiFiles, "ui/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	keys, overview, i18n := read("pages/keys.js"), read("pages/overview.js"), read("i18n.js")
	if !strings.Contains(keys, "k.state === 'orphaned'") || !strings.Contains(keys, "k.state === 'orphaned' ? null : toggle") {
		t.Error("keys.js does not handle orphaned keys")
	}
	if !strings.Contains(overview, "k.state === 'orphaned'") {
		t.Error("overview.js does not count orphaned keys as off")
	}
	if strings.Count(i18n, "'k.orphaned':") != 2 || strings.Count(i18n, "'k.orphanedReason':") != 2 {
		t.Error("i18n.js lacks orphaned messages in both languages")
	}
}

// UI-34：Key 页有批量导入入口，提交到批量接口；页面元素中不渲染密钥；文案中英文齐全。
func TestUIBulkImport(t *testing.T) {
	read := func(name string) string {
		b, err := fs.ReadFile(uiFiles, "ui/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	keys, imp, i18n := read("pages/keys.js"), read("pages/keys_import.js"), read("i18n.js")
	if !strings.Contains(keys, "openImportDialog(") || !strings.Contains(keys, "t('k.import')") {
		t.Error("keys.js has no bulk import entry")
	}
	if !strings.Contains(imp, "api('POST', '/admin/api/keys/import'") {
		t.Error("keys_import.js does not post to the bulk import endpoint")
	}
	for i, line := range strings.Split(imp, "\n") {
		if strings.Contains(line, "h(") && strings.Contains(strings.ToLower(line), "secret") &&
			!strings.Contains(line, "SecretField") && !strings.Contains(line, "secretField") && !strings.Contains(line, "SecretPrefix") {
			t.Errorf("keys_import.js:%d may render a secret: %s", i+1, strings.TrimSpace(line))
		}
	}
	// onClose 与导入完成后都清空文本框（UI-34）
	if strings.Count(imp, "fText.value = '';") < 3 {
		t.Error("keys_import.js does not clear the pasted text on close and after importing")
	}
	used := regexp.MustCompile(`t\('((?:ki\.|k\.import)[\w.]*)'`).FindAllStringSubmatch(imp+keys, -1)
	if len(used) < 20 {
		t.Fatalf("found only %d import messages", len(used))
	}
	for _, m := range used {
		if strings.HasSuffix(m[1], ".") {
			continue // 动态拼接的前缀，例如 'ki.fmt.' + format
		}
		if n := strings.Count(i18n, "'"+m[1]+"':"); n != 2 {
			t.Errorf("message %s defined %d times, want 2 (zh and en)", m[1], n)
		}
	}
	for _, k := range []string{"ki.fmt.lines", "ki.fmt.jsonl", "ki.fmt.csv", "ki.st.invalid", "ki.st.failed"} {
		if strings.Count(i18n, "'"+k+"':") != 2 {
			t.Errorf("message %s missing in a language", k)
		}
	}
}

// UI-35：额度用尽有专门的显示与文案（中英文）；概览把它计入冷却中。
func TestUIQuotaState(t *testing.T) {
	read := func(name string) string {
		b, err := fs.ReadFile(uiFiles, "ui/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	keys, overview, i18n := read("pages/keys.js"), read("pages/overview.js"), read("i18n.js")
	if !strings.Contains(keys, "k.state === 'exhausted'") || !strings.Contains(keys, "quotaCell(k)") {
		t.Error("keys.js does not show quota usage or the exhausted state")
	}
	if !strings.Contains(overview, "k.state === 'exhausted'") {
		t.Error("overview.js does not count exhausted keys")
	}
	for _, k := range []string{"k.quota", "k.quotaResets", "k.quotaNoReset", "k.exhausted", "k.exhaustedReason", "k.exhaustedTotal"} {
		if strings.Count(i18n, "'"+k+"':") != 2 {
			t.Errorf("message %s missing in a language", k)
		}
	}
}

// UI-36：概览读取池级告警并放在最上面；文案中英文齐全。
func TestUIPoolAlerts(t *testing.T) {
	b, err := fs.ReadFile(uiFiles, "ui/pages/overview.js")
	if err != nil {
		t.Fatal(err)
	}
	ov := string(b)
	if !strings.Contains(ov, "api('GET', '/admin/api/alerts')") || !strings.Contains(ov, "poolAlertCard ? [poolAlertCard] : []") {
		t.Error("overview.js does not show pool alerts")
	}
	b, err = fs.ReadFile(uiFiles, "ui/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ov.poolAlerts", "ov.alertCritical", "ov.alertWarning", "ov.alertSince",
		"ov.alert.low_keys", "ov.alert.low_quota", "ov.alert.expiring", "ov.alertDetail.low_keys", "ov.alertDetail.low_quota", "ov.alertDetail.expiring"} {
		if strings.Count(string(b), "'"+k+"':") != 2 {
			t.Errorf("message %s missing in a language", k)
		}
	}
}
