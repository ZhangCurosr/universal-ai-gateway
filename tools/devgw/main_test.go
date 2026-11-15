package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

const sampleConfig = `# my gateway
server:
  data_listen: "0.0.0.0:18496"   # LAN
  admin_listen: "127.0.0.1:18497"
  data_dir: ./data
translation:
  unknown_fields: drop
pools:
  - name: oa
    protocol: openai_chat
    base_url: "fake:openai"   # keep this comment
  - { name: an, protocol: anthropic, base_url: "fake:anthropic" }
  - name: real
    protocol: tavily
    base_url: "https://api.tavily.com"
models:
  - { name: m-oa, pool: oa, price: { input_tokens: "0", output_tokens: "0", cache_read_tokens: "0" } }
  - { name: m-an, pool: an, price: { input_tokens: "0", output_tokens: "0", cache_read_tokens: "0", cache_write_tokens: "0" } }
  - { name: tavily-search, pool: real, price: { credits: "0" } }
`

func TestRewriteConfig(t *testing.T) {
	dir := t.TempDir()
	fakes := map[string]string{"openai": "http://127.0.0.1:1111", "anthropic": "http://127.0.0.1:2222"}
	rw, err := rewriteConfig([]byte(sampleConfig), dir, fakes)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(rw.Data)
	if err != nil {
		t.Fatal(err)
	}
	slash := filepath.ToSlash(dir)
	if cfg.Server.DataDir != slash+"/data" || cfg.Server.MasterKeyFile != slash+"/secrets/master.key" {
		t.Errorf("paths: %q %q", cfg.Server.DataDir, cfg.Server.MasterKeyFile)
	}
	if cfg.Server.DataListen != "127.0.0.1:18496" || rw.DataAddr != "127.0.0.1:18496" || rw.AdminAddr != "127.0.0.1:18497" {
		t.Errorf("listen: %q %q %q", cfg.Server.DataListen, rw.DataAddr, rw.AdminAddr)
	}
	urls := map[string]string{}
	for _, p := range cfg.Pools {
		urls[p.Name] = p.BaseURL
	}
	if urls["oa"] != fakes["openai"] || urls["an"] != fakes["anthropic"] || urls["real"] != "https://api.tavily.com" {
		t.Errorf("base urls: %v", urls)
	}
	if strings.Join(rw.FakePools, ",") != "oa,an" {
		t.Errorf("fake pools: %v", rw.FakePools)
	}
	for _, keep := range []string{"# my gateway", "# keep this comment", "unknown_fields: drop"} {
		if !strings.Contains(string(rw.Data), keep) {
			t.Errorf("lost %q", keep)
		}
	}

	bad := []struct{ name, from, to, want string }{
		{"unknown fake", `"fake:anthropic"`, `"fake:gemini"`, "unknown fake upstream"},
		{"protocol mismatch", `"fake:anthropic"`, `"fake:openai"`, "needs protocol openai_chat"},
		{"not started", `"fake:anthropic"`, `"fake:responses"`, "needs protocol"},
	}
	for _, b := range bad {
		_, err := rewriteConfig([]byte(strings.Replace(sampleConfig, b.from, b.to, 1)), dir, fakes)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: %v", b.name, err)
		}
	}
	if _, err := rewriteConfig([]byte(sampleConfig), dir, map[string]string{"openai": fakes["openai"]}); err == nil || !strings.Contains(err.Error(), "add --fake anthropic") {
		t.Errorf("missing --fake: %v", err)
	}
}

func TestPrepareDir(t *testing.T) {
	base := t.TempDir()
	d, err := prepareDir(filepath.Join(base, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		t.Fatal("dir not created")
	}
	if err := os.WriteFile(filepath.Join(d, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDir(d); err == nil {
		t.Fatal("non-empty dir accepted")
	}
	d2, err := prepareDir("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(d2); err != nil {
		t.Fatal(err)
	}
	if got := splitList(" openai, ,anthropic,openai "); strings.Join(got, ",") != "openai,anthropic" {
		t.Errorf("splitList: %v", got)
	}
}

// 失败退出时同样清理目录；--keep 保留。
func TestRunCleanupAndKeep(t *testing.T) {
	base := t.TempDir()
	cfg := filepath.Join(base, "g.yaml")
	if err := os.WriteFile(cfg, []byte(sampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []bool{false, true} {
		dir := filepath.Join(base, "run"+map[bool]string{false: "0", true: "1"}[keep])
		var out bytes.Buffer
		err := run(context.Background(), options{Config: cfg, Dir: dir, Keep: keep}, &out) // 没有 --fake：配置引用了假上游
		if err == nil || !strings.Contains(err.Error(), "add --fake") {
			t.Fatalf("keep=%v: %v", keep, err)
		}
		_, statErr := os.Stat(dir)
		if keep != (statErr == nil) {
			t.Errorf("keep=%v: dir exists=%v", keep, statErr == nil)
		}
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // 只取端口
	return ln.Addr().String()
}

// 端到端（T77 §5）：编译网关、三种假上游、经网关转发的流式与非流式请求、工具调用、跨协议转译、
// Token 文件、日志中没有 Token、退出后目录被删除、用户的配置文件不变。
func TestDevgwEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the gateway")
	}
	base := t.TempDir()
	cfgText := `server:
  data_listen: "` + freePort(t) + `"
  admin_listen: "` + freePort(t) + `"
translation:
  unknown_fields: drop
pools:
  - { name: oa, protocol: openai_chat, base_url: "fake:openai" }
  - { name: an, protocol: anthropic, base_url: "fake:anthropic" }
  - { name: rs, protocol: openai_responses, base_url: "fake:responses" }
models:
  - { name: m-oa, pool: oa, price: { input_tokens: "0", output_tokens: "0", cache_read_tokens: "0" } }
  - { name: m-an, pool: an, price: { input_tokens: "0", output_tokens: "0", cache_read_tokens: "0", cache_write_tokens: "0" } }
  - { name: m-rs, pool: rs, price: { input_tokens: "0", output_tokens: "0", cache_read_tokens: "0" } }
`
	cfgPath := filepath.Join(base, "g.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "gw")
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	var stdout bytes.Buffer
	ready := func(s *session) {
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join(s.Dir, name))
			if err != nil || !strings.HasPrefix(string(b), "uai_live_") {
				t.Fatalf("%s: %v", name, err)
			}
			if runtime.GOOS != "windows" {
				if fi, err := os.Stat(filepath.Join(s.Dir, name)); err != nil || fi.Mode().Perm() != 0o600 {
					t.Errorf("%s mode: %v %v", name, fi.Mode(), err)
				}
			}
			return string(b)
		}
		admin, client := read("admin.token"), read("client.token")
		tokens = append(tokens, admin, client)
		call := func(name, path, body string, want ...string) {
			req, err := http.NewRequest(http.MethodPost, s.DataURL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+client)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			defer resp.Body.Close() //nolint:errcheck // 测试
			b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Errorf("%s: HTTP %d %s %v", name, resp.StatusCode, b, err)
				return
			}
			for _, w := range want {
				if !strings.Contains(string(b), w) {
					t.Errorf("%s: missing %q in %s", name, w, b)
				}
			}
		}
		user := func(text string) string { return `"messages":[{"role":"user","content":"` + text + `"}]` }
		call("openai", "/v1/chat/completions", `{"model":"m-oa",`+user("hi")+`}`, `"content":"pong"`)
		call("openai stream", "/v1/chat/completions", `{"model":"m-oa","stream":true,`+user("echo:abc")+`}`, `"content":"abc"`, "[DONE]")
		call("openai tool", "/v1/chat/completions", `{"model":"m-oa","tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}],`+user("call:get_time")+`}`, `"name":"get_time"`, `"tool_calls"`)
		call("anthropic", "/v1/messages", `{"model":"m-an","max_tokens":64,`+user("hi")+`}`, `"text":"pong"`)
		call("anthropic stream", "/v1/messages", `{"model":"m-an","max_tokens":64,"stream":true,`+user("echo:xyz")+`}`, "message_start", `"text":"xyz"`, "message_stop")
		call("responses", "/v1/responses", `{"model":"m-rs","input":"hi"}`, `"text":"pong"`)
		call("responses stream", "/v1/responses", `{"model":"m-rs","stream":true,"input":"echo:q"}`, "response.completed", `"delta":"q"`)
		// 跨协议：Claude Code 式请求进 openai 池，Codex 式请求进 openai 池
		call("anthropic->openai", "/v1/messages", `{"model":"m-oa","max_tokens":64,"stream":true,`+user("echo:cross")+`}`, `"text":"cross"`, "message_stop")
		call("responses->openai", "/v1/responses", `{"model":"m-oa","stream":true,"input":"echo:cx"}`, `"delta":"cx"`, "response.completed")

		logText, err := os.ReadFile(filepath.Join(s.Dir, "gateway.log"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(logText), bootstrapPrefix+"<written to admin.token>") {
			t.Error("bootstrap line not redacted in gateway.log")
		}
		fakeText, err := os.ReadFile(filepath.Join(s.Dir, "fake.log"))
		if err != nil || strings.Count(string(fakeText), "\n") < 9 || !strings.Contains(string(fakeText), `"roles":["user"]`) {
			t.Errorf("fake.log: %s %v", fakeText, err)
		}
		for _, tok := range []string{admin, client} {
			if strings.Contains(string(logText), tok) || strings.Contains(string(fakeText), tok) {
				t.Error("token leaked into logs")
			}
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "stop"), nil, 0o600); err != nil { // 停止文件结束 run
			t.Fatal(err)
		}
	}
	err = run(context.Background(), options{Config: cfgPath, Dir: dir, Fakes: []string{"openai", "anthropic", "responses"}, Root: root, Ready: ready}, &stdout)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stdout.String())
	}
	if len(tokens) != 2 {
		t.Fatal("ready not called")
	}
	for _, tok := range tokens {
		if strings.Contains(stdout.String(), tok) {
			t.Error("token printed to the terminal")
		}
	}
	for _, w := range []string{"admin.token", "client.token", "UAIG_URL=", `UAIG_TOKEN="$(cat `, "$env:UAIG_TOKEN=Get-Content"} {
		if !strings.Contains(stdout.String(), w) {
			t.Errorf("summary lacks %q:\n%s", w, stdout.String())
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir not removed: %v", err)
	}
	if b, err := os.ReadFile(cfgPath); err != nil || string(b) != cfgText {
		t.Error("user config changed")
	}
}
