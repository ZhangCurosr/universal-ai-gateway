package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var listenPath = []string{"server", "data_listen"}

// LAN-03：只改这一处文本，注释、空行与其他字段原样保留。
func TestSetScalarReplacesOnlyTheValue(t *testing.T) {
	cases := map[string]struct{ in, want, old string }{
		"double quoted with comment": {
			"# 网关\nserver:\n  data_listen: \"0.0.0.0:8080\"   # 数据面\n\n  admin_listen: 127.0.0.1:8081\n",
			"# 网关\nserver:\n  data_listen: \"127.0.0.1:8080\"   # 数据面\n\n  admin_listen: 127.0.0.1:8081\n", "0.0.0.0:8080"},
		"plain": {
			"server:\n    data_listen: 0.0.0.0:8080 # c\n",
			"server:\n    data_listen: \"127.0.0.1:8080\" # c\n", "0.0.0.0:8080"},
		"single quoted, no newline at end": {
			"server:\n  data_listen: '0.0.0.0:8080'",
			"server:\n  data_listen: \"127.0.0.1:8080\"", "0.0.0.0:8080"},
		"missing field": {
			"server:\n  # 注释\n  log_level: info\npools: []\n",
			"server:\n  # 注释\n  data_listen: \"127.0.0.1:8080\"\n  log_level: info\npools: []\n", ""},
		"empty server": {
			"server:\npools: []\n",
			"server:\n  data_listen: \"127.0.0.1:8080\"\npools: []\n", ""},
		"missing server": {
			"pools: []",
			"pools: []\nserver:\n  data_listen: \"127.0.0.1:8080\"\n", ""},
		"empty file": {"", "server:\n  data_listen: \"127.0.0.1:8080\"\n", ""},
	}
	for name, c := range cases {
		out, old, err := SetScalar([]byte(c.in), listenPath, "127.0.0.1:8080")
		if err != nil || string(out) != c.want || old != c.old {
			t.Errorf("%s:\n got %q (old %q, err %v)\nwant %q", name, out, old, err, c.want)
		}
	}
}

func TestSetScalarRejects(t *testing.T) {
	for name, in := range map[string]string{
		"flow mapping":   "server: { data_listen: \"0.0.0.0:8080\" }\n",
		"not a mapping":  "server: 3\n",
		"block scalar":   "server:\n  data_listen: |\n    0.0.0.0:8080\n",
		"top-level list": "- a\n",
		"bad yaml":       "server: [\n",
	} {
		if _, _, err := SetScalar([]byte(in), listenPath, "x"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := SetScalar([]byte("a: 1"), nil, "x"); err == nil {
		t.Error("empty path accepted")
	}
}

func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	orig := "# keep me\nserver:\n  data_listen: \"0.0.0.0:18080\"\n  data_dir: ./data\n" +
		"pools:\n  - name: tv\n    protocol: tavily\n    base_url: \"https://api.tavily.com\"\nmodels:\n  - name: s\n    pool: tv\n    price: { credits: \"0\" }\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	got, old, err := EditFile(path, listenPath, "127.0.0.1:18080")
	if err != nil || string(got) != orig || old != "0.0.0.0:18080" {
		t.Fatalf("EditFile = %q %q %v", got, old, err)
	}
	now, _ := os.ReadFile(path)
	bak, _ := os.ReadFile(path + ".bak")
	if string(now) != strings.Replace(orig, "0.0.0.0", "127.0.0.1", 1) || string(bak) != orig {
		t.Fatalf("file = %q, backup = %q", now, bak)
	}
	// 值相同：不写文件
	if _, _, err := EditFile(path, listenPath, "127.0.0.1:18080"); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("unchanged: %v", err)
	}
	// 新内容不合法：不写文件
	if _, _, err := EditFile(path, listenPath, "not an address"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(now) {
		t.Fatal("invalid edit changed the file")
	}
	if _, _, err := EditFile(filepath.Join(dir, "missing.yaml"), listenPath, "x"); err == nil {
		t.Fatal("missing file accepted")
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 2 {
		t.Fatalf("leftover files: %d", len(des))
	}
}
