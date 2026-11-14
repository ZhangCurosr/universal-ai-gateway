package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

const connectSecret = "uai_live_CONNECT_SECRET_VALUE"

func connectTestEnv(t *testing.T, token string) (connectEnv, string, string) {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	vars := map[string]string{envURL: "http://192.168.1.5:8080/", envToken: token}
	return connectEnv{
		getenv: func(k string) string { return vars[k] },
		home:   home, cwd: cwd,
		models: func(_ context.Context, base, tok string) ([]string, error) {
			if base != "http://192.168.1.5:8080" || tok != token {
				t.Errorf("models(%q, token ok=%v)", base, tok == token)
			}
			return []string{"claude-sonnet", "gpt-mini"}, nil
		},
	}, home, cwd
}

func runC(t *testing.T, env connectEnv, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := runConnect(context.Background(), args, env, &out, &errOut)
	if strings.Contains(out.String()+errOut.String(), connectSecret) {
		t.Fatalf("%v: token value in output", args)
	}
	return code, out.String(), errOut.String()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if strings.Contains(string(b), connectSecret) {
		t.Fatalf("%s contains the token value", p)
	}
	return string(b)
}

// CN-02 ~ CN-05、CN-07：四个 CLI 写出的文件、技能目录；文件与输出中没有 Token 值（CN-01）。
func TestConnectWrites(t *testing.T) {
	cases := []struct {
		cli, scope string
		extra      []string
		file       func(home, cwd string) string
		skill      func(home, cwd string) string
		want       []string
	}{
		{"claude", "project", nil, func(_, c string) string { return filepath.Join(c, ".mcp.json") }, func(_, c string) string { return filepath.Join(c, ".claude", "skills") },
			[]string{`"mcpServers": {`, `"uaig": {`, `"url": "http://192.168.1.5:8080/mcp"`, `"Authorization": "Bearer ${UAIG_TOKEN}"`, `"type": "http"`}},
		{"codex", "user", nil, func(h, _ string) string { return filepath.Join(h, ".codex", "config.toml") }, func(h, _ string) string { return filepath.Join(h, ".agents", "skills") },
			[]string{tomlBegin, "[model_providers.uaig]", `base_url = "http://192.168.1.5:8080/v1"`, `env_key = "UAIG_TOKEN"`, `wire_api = "responses"`, "[mcp_servers.uaig]", `bearer_token_env_var = "UAIG_TOKEN"`, tomlEnd}},
		{"opencode", "project", []string{"--opencode-schema", "v2"}, func(_, c string) string { return filepath.Join(c, "opencode.json") }, func(_, c string) string { return filepath.Join(c, ".agents", "skills") },
			[]string{`"$schema": "https://opencode.ai/config.json"`, `"providers": {`, `"package": "@opencode/ai/providers/openai-compatible"`, `"apiKey": "{env:UAIG_TOKEN}"`, `"gpt-mini": {`, `"servers": {`, `"Authorization": "Bearer {env:UAIG_TOKEN}"`, `"oauth": false`}},
		{"opencode", "user", []string{"--opencode-schema", "v1"}, func(h, _ string) string { return filepath.Join(h, ".config", "opencode", "opencode.json") }, func(h, _ string) string { return filepath.Join(h, ".agents", "skills") },
			[]string{`"provider": {`, `"npm": "@ai-sdk/openai-compatible"`, `"baseURL": "http://192.168.1.5:8080/v1"`, `"mcp": {`, `"type": "remote"`}},
		{"pi", "project", nil, func(h, _ string) string { return filepath.Join(h, ".pi", "agent", "models.json") }, func(_, c string) string { return filepath.Join(c, ".agents", "skills") },
			[]string{`"providers": {`, `"baseUrl": "http://192.168.1.5:8080/v1"`, `"api": "openai-completions"`, `"apiKey": "$UAIG_TOKEN"`, `"id": "claude-sonnet"`}},
		{"pi", "user", []string{"--pi-api", "anthropic-messages"}, func(h, _ string) string { return filepath.Join(h, ".pi", "agent", "models.json") }, func(h, _ string) string { return filepath.Join(h, ".agents", "skills") },
			[]string{`"baseUrl": "http://192.168.1.5:8080"`, `"api": "anthropic-messages"`}},
	}
	for _, c := range cases {
		env, home, cwd := connectTestEnv(t, connectSecret)
		args := append([]string{c.cli, "--scope", c.scope, "--write"}, c.extra...)
		code, out, errOut := runC(t, env, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
		got := readFile(t, c.file(home, cwd))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%v: file lacks %q:\n%s", args, w, got)
			}
		}
		skill := readFile(t, filepath.Join(c.skill(home, cwd), "use-gateway", "SKILL.md"))
		if !strings.HasPrefix(skill, "---\nname: use-gateway") || !strings.Contains(skill, "installed by `gateway connect`") {
			t.Errorf("%v: skill = %.80q", args, skill)
		}
		if !strings.Contains(out, "setx UAIG_TOKEN") && !strings.Contains(out, "export UAIG_TOKEN") {
			t.Errorf("%v: no hint for setting UAIG_TOKEN", args)
		}
	}
}

func TestConnectPrintOnlyAndPlaceholders(t *testing.T) {
	env, home, cwd := connectTestEnv(t, "")
	code, out, errOut := runC(t, env, "codex")
	if code != 0 || !strings.Contains(out, "[model_providers.uaig]") || !strings.Contains(out, "nothing was written") || !strings.Contains(errOut, "UAIG_TOKEN is not set") ||
		!strings.Contains(out, "model = \""+modelHolder+"\"") {
		t.Fatalf("print only: %d %s %s", code, out, errOut)
	}
	for _, d := range []string{home, cwd} {
		if des, _ := os.ReadDir(d); len(des) != 0 {
			t.Fatalf("%s: files written without --write", d)
		}
	}
	if code, out, _ := runC(t, env, "list"); code != 0 || strings.Count(out, "\n") != 4 {
		t.Fatalf("list: %d %q", code, out)
	}
	for _, args := range [][]string{{}, {"vim"}, {"codex", "--scope", "x"}, {"pi", "--pi-api", "x"}, {"opencode", "--opencode-schema", "v9"}, {"codex", "extra"}} {
		if code, _, _ := runC(t, env, args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// CN-06：已有 JSON 只改 uaig 条目，其他内容与键顺序不变；JSONC 不写；幂等；备份。
func TestConnectMergesJSON(t *testing.T) {
	env, _, cwd := connectTestEnv(t, connectSecret)
	path := filepath.Join(cwd, ".mcp.json")
	orig := "{\n  \"zeta\": 1,\n  \"mcpServers\": {\n    \"other\": {\"command\": \"x\", \"args\": [\"a\", 2.50]},\n    \"uaig\": {\"url\": \"old\"}\n  },\n  \"alpha\": [true, null]\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runC(t, env, "claude", "--write"); code != 0 {
		t.Fatal(errOut)
	}
	got := readFile(t, path)
	iz, im, io, iu, ia := strings.Index(got, `"zeta"`), strings.Index(got, `"mcpServers"`), strings.Index(got, `"other"`), strings.Index(got, `"uaig"`), strings.Index(got, `"alpha"`)
	if !(iz >= 0 && iz < im && im < io && io < iu && iu < ia) || !strings.Contains(got, "2.50") || strings.Contains(got, `"old"`) || !strings.Contains(got, `"command": "x"`) {
		t.Fatalf("merged = %s", got)
	}
	if bak := readFile(t, path+".bak"); bak != orig {
		t.Fatalf("backup = %q", bak)
	}
	_, out, _ := runC(t, env, "claude", "--write")
	if !strings.Contains(out, "unchanged") || readFile(t, path) != got || readFile(t, path+".bak") != orig {
		t.Fatalf("not idempotent: %s", out)
	}
	jsonc := "{\n  // comment\n  \"mcpServers\": {}\n}\n"
	if err := os.WriteFile(path, []byte(jsonc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ = runC(t, env, "claude", "--write")
	if !strings.Contains(out, "not plain JSON") || readFile(t, path) != jsonc {
		t.Fatalf("jsonc: %s", out)
	}
}

// CN-03、CN-06：TOML 标记块追加、替换、幂等，块外内容不变。
func TestConnectTOMLBlock(t *testing.T) {
	env, home, _ := connectTestEnv(t, connectSecret)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	head := "model = \"o3\"\n\n[profiles.x]\nfoo = 1"
	if err := os.WriteFile(path, []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runC(t, env, "codex", "--scope", "user", "--write"); code != 0 {
		t.Fatal(errOut)
	}
	first := readFile(t, path)
	if !strings.HasPrefix(first, head+"\n\n"+tomlBegin+"\n") || strings.Count(first, tomlBegin) != 1 {
		t.Fatalf("appended = %q", first)
	}
	// 改地址后再写：替换块，块外不变
	vars := map[string]string{envURL: "http://10.0.0.9:9000", envToken: connectSecret}
	env.getenv = func(k string) string { return vars[k] }
	env.models = nil
	runC(t, env, "codex", "--scope", "user", "--write")
	second := readFile(t, path)
	if strings.Count(second, tomlBegin) != 1 || !strings.Contains(second, "http://10.0.0.9:9000/v1") || strings.Contains(second, "192.168.1.5") || !strings.HasPrefix(second, head) {
		t.Fatalf("replaced = %q", second)
	}
	if replaceTOMLBlock(second, strings.TrimSuffix(strings.SplitN(strings.SplitN(second, tomlBegin+"\n", 2)[1], tomlEnd, 2)[0], "\n")) != second {
		t.Fatal("not idempotent")
	}
}

// fetchModels 对真实网关：只返回能当作大模型使用的服务（ask_model 的可用服务），不含 REST 服务。
func TestConnectFetchModels(t *testing.T) {
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools,
			config.PoolConfig{Name: "gem", Protocol: "gemini", BaseURL: "http://127.0.0.1:1"},
			config.PoolConfig{Name: "rs", Protocol: "rest", BaseURL: "http://127.0.0.1:1"})
		c.Models = append(c.Models,
			config.ModelConfig{Name: "gem-pro", Pool: "gem", Price: map[string]string{"input_tokens": "1/1M", "output_tokens": "1/1M", "cache_read_tokens": "0.1/1M"}},
			config.ModelConfig{Name: "rest-svc", Pool: "rs", Price: map[string]string{"requests": "0"}})
		c.Normalize()
	})
	var cl struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", "http://"+g.admin+"/admin/api/clients", g.adminToken, map[string]any{"name": "c", "role": "user", "quota_unlimited": true}, &cl)
	got, err := fetchModels(context.Background(), "http://"+g.data, cl.Token)
	if err != nil || strings.Join(got, ",") != "gem-pro" {
		t.Fatalf("models = %v, %v", got, err)
	}
	if _, err := fetchModels(context.Background(), "http://"+g.data, "uai_live_wrong"); err == nil {
		t.Fatal("wrong token accepted")
	}
}

// CN-09：各 CLI 用户级配置目录的环境变量优先于缺省位置。
func TestConnectHonorsConfigDirVars(t *testing.T) {
	env, home, _ := connectTestEnv(t, connectSecret)
	alt := t.TempDir()
	vars := map[string]string{envURL: "http://127.0.0.1:8080", envToken: connectSecret,
		"PI_CODING_AGENT_DIR": filepath.Join(alt, "pi"), "CODEX_HOME": filepath.Join(alt, "codex"),
		"XDG_CONFIG_HOME": filepath.Join(alt, "xdg"), "CLAUDE_CONFIG_DIR": filepath.Join(alt, "claude")}
	env.getenv = func(k string) string { return vars[k] }
	env.models = nil
	for _, c := range []struct{ cli, want string }{
		{"pi", filepath.Join(alt, "pi", "models.json")},
		{"codex", filepath.Join(alt, "codex", "config.toml")},
		{"opencode", filepath.Join(alt, "xdg", "opencode", "opencode.json")},
		{"claude", filepath.Join(alt, "claude", "skills", "use-gateway", "SKILL.md")},
	} {
		if code, _, errOut := runC(t, env, c.cli, "--scope", "user", "--write", "--opencode-schema", "v2"); code != 0 {
			t.Fatalf("%s: %s", c.cli, errOut)
		}
		readFile(t, c.want)
	}
	if des, _ := os.ReadDir(filepath.Join(home, ".pi")); len(des) != 0 {
		t.Fatal("wrote to the default pi directory")
	}
}
