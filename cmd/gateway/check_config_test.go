package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T76（CK-01 ~ CK-04）：check-config 查出原来要到启动时才报的错误；不创建数据目录；--json。
func TestCheckConfigFull(t *testing.T) {
	dir := t.TempDir()
	head := "server:\n  data_dir: " + filepath.ToSlash(filepath.Join(dir, "data")) + "\n  master_key_file: " + filepath.ToSlash(filepath.Join(dir, "secrets", "master.key")) + "\n"
	cases := []struct {
		name, body, want string
	}{
		{"missing cache_read_tokens", "pools:\n  - { name: ds, protocol: openai_chat, base_url: \"https://api.deepseek.com\" }\nmodels:\n  - { name: deepseek-chat, pool: ds, price: { input_tokens: \"1/1M\", output_tokens: \"1/1M\" } }\n",
			`unit "cache_read_tokens"`},
		{"mineru requests", "pools:\n  - { name: mu, protocol: mineru, base_url: \"https://mineru.net\" }\nmodels:\n  - { name: mineru-parse, pool: mu, price: { pages: \"0.01\" } }\n",
			`unit "requests"`},
		{"tools on a protocol without declarations", "pools:\n  - { name: tv, protocol: tavily, base_url: \"https://api.tavily.com\" }\nmodels:\n  - name: tavily-search\n    pool: tv\n    price: { credits: \"0\" }\n    tools:\n      - { name: x_tool, description: d, method: GET, path: /x }\n",
			"tools"},
		{"webhook file", "alerts:\n  webhook: { url_file: " + filepath.ToSlash(filepath.Join(dir, "missing.url")) + " }\npools:\n  - { name: tv, protocol: tavily, base_url: \"https://api.tavily.com\" }\nmodels:\n  - { name: tavily-search, pool: tv, price: { credits: \"0\" } }\n",
			"missing.url"},
	}
	run := func(args ...string) (int, string, string) {
		var out, errOut strings.Builder
		code := cmdCheckConfig(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	for i, c := range cases {
		p := filepath.Join(dir, "c"+string(rune('0'+i))+".yaml")
		if err := os.WriteFile(p, []byte(head+c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := run("--config", p)
		if code != 1 || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: %d %q %q", c.name, code, out, errOut)
		}
		code, out, _ = run("--config", p, "--json")
		if code != 1 || !strings.HasPrefix(out, `{"error":`) || !strings.Contains(out, `"ok":false`) {
			t.Errorf("%s --json: %d %q", c.name, code, out)
		}
	}
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte(head+"pools:\n  - { name: ds, protocol: openai_chat, base_url: \"https://api.deepseek.com\" }\nmodels:\n  - { name: deepseek-chat, pool: ds, price: { input_tokens: \"1/1M\", output_tokens: \"1/1M\", cache_read_tokens: \"0.1/1M\" } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run("--config", good); code != 0 || out != "config ok\n" {
		t.Fatalf("good: %d %q %q", code, out, errOut)
	}
	if code, out, _ := run("--config", good, "--json"); code != 0 || strings.TrimSpace(out) != `{"ok":true}` {
		t.Fatalf("good --json: %d %q", code, out)
	}
	for _, d := range []string{"data", "secrets"} { // CK-02：不创建任何文件
		if _, err := os.Stat(filepath.Join(dir, d)); !os.IsNotExist(err) {
			t.Errorf("%s was created", d)
		}
	}
	if code, _, _ := run("--config", filepath.Join(dir, "none.yaml")); code != 1 {
		t.Fatal("missing file accepted")
	}
	if code, _, _ := run("--bogus"); code != 2 {
		t.Fatal("bad flag accepted")
	}
}
