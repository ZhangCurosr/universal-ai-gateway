package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// CLI-09：密钥导入，任何输出都不包含密钥与文件中的其他值
func TestKeysImport(t *testing.T) {
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "imp", Protocol: "rest", BaseURL: "http://127.0.0.1:1"})
		c.Models = append(c.Models, config.ModelConfig{Name: "imp-svc", Pool: "imp", Price: map[string]string{"requests": "0"}})
		c.Normalize()
	})
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "accounts.jsonl")
	secretsInFile := []string{"ory_at_SECRETONE", "ory_at_SECRETTWO", "ory_at_SECRETBAD", "alice@example.com", "hunter2", "bob@example.com"}
	content := strings.Join([]string{
		`{"email":"alice@example.com","password":"hunter2","status":"ok","api_key":"ory_at_SECRETONE"}`,
		`{"email":"bob@example.com","status":"ok","api_key":"ory_at_SECRETTWO"}`,
		`{"email":"x@example.com","status":"banned","api_key":"ory_at_SECRETBAD"}`,
		`not json at all`,
		`{"status":"ok","api_key":""}`,
		`{"status":"ok"}`,
		``,
		`{"status":"ok","api_key":"ory_at_SECRETONE"}`,
	}, "\n")
	if err := os.WriteFile(jsonl, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		return map[string]string{"UAIG_ADMIN_URL": "http://" + g.admin, "UAIG_ADMIN_TOKEN": g.adminToken}[k]
	}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := realMainIO(append([]string{"keys", "import"}, args...), strings.NewReader(""), &out, &errb, env)
		return code, out.String(), errb.String()
	}
	noLeak := func(s string) {
		t.Helper()
		for _, x := range append(secretsInFile, g.adminToken) {
			if strings.Contains(s, x) {
				t.Errorf("output leaks %q:\n%s", x, s)
			}
		}
	}

	code, out, errOut := run("--pool", "imp", "--file", jsonl, "--where", "status=ok", "--secret-prefix", "bearer:", "--dry-run")
	noLeak(out + errOut)
	if code != 0 || !strings.Contains(out, "would import 3, duplicate 0, skipped 4") {
		t.Fatalf("dry-run: %d %q %q", code, out, errOut)
	}
	if n := len(listKeys(t, g)); n != 0 {
		t.Fatalf("dry-run added %d keys", n)
	}

	code, out, errOut = run("--pool", "imp", "--file", jsonl, "--where", "status=ok", "--secret-prefix", "bearer:", "--name-prefix", "bv")
	noLeak(out + errOut)
	if code != 0 || !strings.Contains(out, "imported 2, duplicate 1, skipped 4") {
		t.Fatalf("import: %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"line 3: skipped (filtered by --where)", "line 4: skipped (not a JSON object)", "line 5: skipped (api_key is not a non-empty string)", "line 6: skipped (no api_key field)"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	keys := listKeys(t, g)
	if len(keys) != 2 || keys[0].Name != "bv-001" || keys[1].Name != "bv-002" || keys[0].Pool != "imp" {
		t.Fatalf("keys = %+v", keys)
	}

	lines := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(lines, []byte("# comment\n\nlineSecretAAAA1111\nlineSecretBBBB2222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = run("--pool", "imp", "--file", lines, "--format", "lines")
	if code != 0 || !strings.Contains(out, "imported 2") || strings.Contains(out+errOut, "lineSecret") {
		t.Fatalf("lines: %d %q %q", code, out, errOut)
	}

	code, _, errOut = run("--pool", "nosuch", "--file", lines, "--format", "lines")
	if code != 1 || !strings.Contains(errOut, `HTTP 400: pool "nosuch" is not configured`) || strings.Contains(errOut, "lineSecret") {
		t.Fatalf("unknown pool: %d %q", code, errOut)
	}

	var out2, err2 bytes.Buffer
	if code := realMainIO([]string{"keys", "import", "--pool", "imp", "--file", lines}, strings.NewReader(""), &out2, &err2, func(string) string { return "" }); code != 2 || !strings.Contains(err2.String(), "UAIG_ADMIN_TOKEN") {
		t.Fatalf("missing token: %d %q", code, err2.String())
	}
	if code, _, errOut := run("--pool", "imp"); code != 2 || !strings.Contains(errOut, "--file") {
		t.Fatalf("missing --file: %d %q", code, errOut)
	}
	if code, _, _ := run("--pool", "imp", "--file", filepath.Join(dir, "missing")); code != 1 {
		t.Fatalf("missing file: %d", code)
	}
	if strings.Contains(g.stderr.String(), "SECRETONE") || strings.Contains(g.stderr.String(), "lineSecret") {
		t.Error("gateway log leaks imported keys")
	}
}

func listKeys(t *testing.T, g *restGateway) []keyViewItem {
	t.Helper()
	var out struct {
		Keys []keyViewItem `json:"keys"`
	}
	doJSON(t, "GET", "http://"+g.admin+"/admin/api/keys", g.adminToken, nil, &out)
	var mine []keyViewItem
	for _, k := range out.Keys {
		if k.Pool == "imp" {
			mine = append(mine, k)
		}
	}
	return mine
}

func TestExtractSecretNeverEchoesContent(t *testing.T) {
	_, ok, why := extractSecret(`{"api_key": 12345, "password":"p@ss"}`, "jsonl", "api_key", nil)
	if ok || strings.Contains(why, "12345") || strings.Contains(why, "p@ss") {
		t.Fatalf("why = %q", why)
	}
	s, ok, _ := extractSecret(`  {"k":"  v  ","on":true} `, "jsonl", "k", map[string]string{"on": "true"})
	if !ok || s != "v" {
		t.Fatalf("secret = %q, %v", s, ok)
	}
}
