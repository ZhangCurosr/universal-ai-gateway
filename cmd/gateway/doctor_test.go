package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

const (
	drGood  = "uai_live_DOCTOR_GOOD_TOKEN_VALUE"
	drEmpty = "uai_live_DOCTOR_EMPTY_TOKEN_VALUE"
)

// newDoctorGateway 是假网关：/healthz、/v1/models（按 Token：正常 / 空列表 / 401）、/mcp（目录模式）。
func newDoctorGateway(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + drGood:
			w.Write([]byte(`{"object":"list","data":[{"id":"a"},{"id":"b"}]}`)) //nolint:errcheck // 测试
		case "Bearer " + drEmpty:
			w.Write([]byte(`{"object":"list","data":[]}`)) //nolint:errcheck // 测试
		default:
			http.Error(w, `{"error":{"message":"invalid token"}}`, http.StatusUnauthorized)
		}
	})
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var result any
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": mcp.CatalogSearchTool, "description": "s", "inputSchema": map[string]any{"type": "object"}},
				{"name": mcp.CatalogCallTool, "description": "c", "inputSchema": map[string]any{"type": "object"}},
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) //nolint:errcheck // 测试
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type drResult struct {
	Checks []drCheck `json:"checks"`
	OK     bool      `json:"ok"`
}

func runDR(t *testing.T, vars map[string]string, home, cwd string, args ...string) (int, drResult, string) {
	t.Helper()
	env := doctorEnv{getenv: func(k string) string { return vars[k] }, home: home, cwd: cwd, http: &http.Client{}}
	var out, errOut strings.Builder
	code := runDoctor(context.Background(), append(args, "--json"), env, &out, &errOut)
	var text strings.Builder
	runDoctor(context.Background(), args, env, &text, &errOut) // 文本输出同样不能出现 Token
	all := out.String() + errOut.String() + text.String()
	for _, secret := range []string{drGood, drEmpty, "uai_live_WRONG_TOKEN_VALUE", "uai_live_HARDCODED_VALUE_123"} {
		if strings.Contains(all, secret) {
			t.Fatalf("token value in output: %s", all)
		}
	}
	var r drResult
	if err := json.Unmarshal([]byte(out.String()), &r); err != nil {
		t.Fatalf("json: %v %q", err, out.String())
	}
	return code, r, text.String()
}

func statuses(r drResult) string {
	var s []string
	for _, c := range r.Checks {
		s = append(s, c.ID+"="+c.Status)
	}
	return strings.Join(s, " ")
}

// DR-01 ~ DR-04、DR-07：各项的通过 / 失败分支、失败后的跳过、退出码。
func TestDoctorNetworkChecks(t *testing.T) {
	gw := newDoctorGateway(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + ln.Addr().String()
	ln.Close() //nolint:errcheck // 关闭后该端口拒绝连接

	cases := []struct {
		name string
		vars map[string]string
		code int
		want string
		msg  string // 某一项消息中应出现的文字
	}{
		{"no env", map[string]string{}, 1, "DR-01=fail DR-02=fail DR-03=skip DR-04=skip", "UAIG_TOKEN is not set"},
		{"bad url", map[string]string{envURL: "ftp://x", envToken: drGood}, 1, "DR-01=fail DR-02=skip DR-03=skip DR-04=skip", "must look like"},
		{"bad token prefix", map[string]string{envURL: gw.URL, envToken: "sk-abc"}, 1, "DR-01=fail DR-02=pass DR-03=skip DR-04=skip", "does not start with uai_live_"},
		{"refused", map[string]string{envURL: refused, envToken: drGood}, 1, "DR-01=pass DR-02=fail DR-03=skip DR-04=skip", "connection refused"},
		{"lan refused hint", map[string]string{envURL: "http://10.255.255.1:1", envToken: drGood}, 1, "DR-01=pass DR-02=fail DR-03=skip DR-04=skip", "LAN sharing"},
		{"401", map[string]string{envURL: gw.URL, envToken: "uai_live_WRONG_TOKEN_VALUE"}, 1, "DR-01=pass DR-02=pass DR-03=fail DR-04=skip", "rejected the token"},
		{"no services", map[string]string{envURL: gw.URL, envToken: drEmpty}, 1, "DR-01=pass DR-02=pass DR-03=fail DR-04=skip", "allowed_models"},
		{"ok", map[string]string{envURL: gw.URL + "/", envToken: drGood}, 0, "DR-01=pass DR-02=pass DR-03=pass DR-04=pass", "catalog mode"},
		{"admin address", map[string]string{envURL: gw.URL + "/nothing", envToken: drGood}, 1, "DR-01=pass DR-02=fail DR-03=skip DR-04=skip", "HTTP 404"},
	}
	for _, c := range cases {
		code, r, text := runDR(t, c.vars, t.TempDir(), t.TempDir())
		if code != c.code || statuses(r) != c.want || r.OK != (c.code == 0) {
			t.Errorf("%s: code %d ok %v %s\n%s", c.name, code, r.OK, statuses(r), text)
			continue
		}
		if !strings.Contains(text, c.msg) {
			t.Errorf("%s: output lacks %q:\n%s", c.name, c.msg, text)
		}
	}
	// URL 未设置只是警告（使用缺省地址）
	_, r, _ := runDR(t, map[string]string{envToken: drGood}, t.TempDir(), t.TempDir())
	if r.Checks[0].Status != drWarn {
		t.Errorf("unset URL: %s", statuses(r))
	}
}

// DR-05、DR-06：CLI 配置的四种情况（不存在、地址不一致、写死 Token、正常），技能与 Claude Code 的环境变量。
func TestDoctorCLIConfig(t *testing.T) {
	gw := newDoctorGateway(t)
	vars := map[string]string{envURL: gw.URL, envToken: drGood}
	home, cwd := t.TempDir(), t.TempDir()
	find := func(r drResult, id string) drCheck {
		for _, c := range r.Checks {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("no %s in %s", id, statuses(r))
		return drCheck{}
	}

	// 1. 不存在
	code, r, _ := runDR(t, vars, home, cwd, "--cli", "codex")
	if c := find(r, "DR-05 codex"); code != 1 || c.Status != drFail || !strings.Contains(c.Fix, "gateway connect codex --write") {
		t.Fatalf("missing: %d %+v", code, c)
	}

	// 2. 正常：用 gateway connect 写出的配置（user 级），技能版本一致
	cenv := connectEnv{getenv: func(k string) string { return vars[k] }, home: home, cwd: cwd}
	var out, errOut strings.Builder
	if code := runConnect(context.Background(), []string{"codex", "--scope", "user", "--write"}, cenv, &out, &errOut); code != 0 {
		t.Fatalf("connect: %s", errOut.String())
	}
	cfg := filepath.Join(home, ".codex", "config.toml")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append([]byte("model_provider = \"uaig\"\n"), b...), 0o600); err != nil {
		t.Fatal(err)
	}
	code, r, text := runDR(t, vars, home, cwd, "--cli", "codex")
	if c := find(r, "DR-05 codex"); code != 0 || c.Status != drPass || find(r, "DR-06").Status != drPass {
		t.Fatalf("good: %d %s\n%s", code, statuses(r), text)
	}

	// 3. 地址不一致
	diff := map[string]string{envURL: "http://127.0.0.1:1", envToken: drGood}
	_, r, text = runDR(t, diff, home, cwd, "--cli", "codex")
	if c := find(r, "DR-05 codex"); c.Status != drFail || !strings.Contains(c.Message, "differs from") || !strings.Contains(c.Message, gw.URL) {
		t.Fatalf("differs: %s\n%s", statuses(r), text)
	}

	// 4. 写死 Token（DR-06 警告，不输出值）；没有引用 UAIG_TOKEN → 警告
	pi := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(pi), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pi, []byte(`{"providers":{"uaig":{"baseUrl":"`+gw.URL+`/v1","apiKey":"uai_live_HARDCODED_VALUE_123"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, r, text = runDR(t, vars, home, cwd, "--cli", "pi")
	if c := find(r, "DR-05 pi"); c.Status != drWarn || !strings.Contains(c.Message, "does not reference UAIG_TOKEN") {
		t.Fatalf("pi: %s\n%s", statuses(r), text)
	}
	if c := find(r, "DR-06"); c.Status != drWarn || !strings.Contains(c.Message, "models.json") || code != 0 {
		t.Fatalf("hardcoded: %d %+v", code, c)
	}

	// PI_CODING_AGENT_DIR（CN-09）：看的是这个目录，不再是 ~/.pi/agent
	vars["PI_CODING_AGENT_DIR"] = t.TempDir()
	_, r, _ = runDR(t, vars, home, cwd, "--cli", "pi")
	if c := find(r, "DR-05 pi"); c.Status != drFail || !strings.Contains(c.Message, vars["PI_CODING_AGENT_DIR"]) {
		t.Fatalf("pi dir: %+v", c)
	}
	delete(vars, "PI_CODING_AGENT_DIR")

	// 技能版本旧 → 警告
	skill := filepath.Join(home, ".agents", "skills", "use-gateway", "SKILL.md")
	sb, err := os.ReadFile(skill)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte(strings.Replace(string(sb), "(gateway "+version+")", "(gateway v0.0.1-old)", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, r, _ = runDR(t, vars, home, cwd, "--cli", "codex")
	if c := find(r, "DR-05 codex"); c.Status != drWarn || !strings.Contains(c.Message, "v0.0.1-old") {
		t.Fatalf("old skill: %+v", c)
	}

	// Claude Code：环境变量
	_, r, _ = runDR(t, vars, home, cwd, "--cli", "claude")
	if c := find(r, "DR-05 claude"); c.Status != drFail || !strings.Contains(c.Message, "ANTHROPIC_BASE_URL is not set") {
		t.Fatalf("claude none: %+v", c)
	}
	vars["ANTHROPIC_BASE_URL"], vars["ANTHROPIC_AUTH_TOKEN"] = gw.URL, "uai_live_WRONG_TOKEN_VALUE"
	_, r, _ = runDR(t, vars, home, cwd, "--cli", "claude")
	if c := find(r, "DR-05 claude"); c.Status != drWarn || !strings.Contains(c.Message, "ANTHROPIC_AUTH_TOKEN differs") {
		t.Fatalf("claude token differs: %+v", c)
	}
	vars["ANTHROPIC_BASE_URL"] = "http://elsewhere:1"
	_, r, _ = runDR(t, vars, home, cwd, "--cli", "claude")
	if c := find(r, "DR-05 claude"); c.Status != drFail {
		t.Fatalf("claude url differs: %+v", c)
	}

	// --cli all：四项都在；参数错误
	_, r, _ = runDR(t, vars, home, cwd, "--cli", "all")
	for _, c := range connectCLIs {
		find(r, "DR-05 "+c)
	}
	env := doctorEnv{getenv: func(string) string { return "" }, home: home, cwd: cwd, http: &http.Client{}}
	var o, e strings.Builder
	if runDoctor(context.Background(), []string{"--cli", "vim"}, env, &o, &e) != 2 || runDoctor(context.Background(), []string{"extra"}, env, &o, &e) != 2 {
		t.Fatal("bad args accepted")
	}
}

// 只读：doctor 不创建、不修改任何文件。
func TestDoctorReadOnly(t *testing.T) {
	gw := newDoctorGateway(t)
	home, cwd := t.TempDir(), t.TempDir()
	runDR(t, map[string]string{envURL: gw.URL, envToken: drGood}, home, cwd, "--cli", "all")
	for _, d := range []string{home, cwd} {
		entries, err := os.ReadDir(d)
		if err != nil || len(entries) != 0 {
			t.Errorf("%s: %d entries %v", d, len(entries), err)
		}
	}
}
