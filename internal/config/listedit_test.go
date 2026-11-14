package config

import (
	"strings"
	"testing"
)

const listDoc = `# gateway
server:
  data_dir: ./data

# 池
pools:
  # 主力
  - name: a
    protocol: tavily   # 搜索
    base_url: "https://api.tavily.com"

  - name: b
    protocol: rest
    base_url: "https://x"
    # b 的注释
    max_attempts: 2

# 服务
models:
  - name: s1
    pool: a
    price: { credits: "0.008" }
`

func TestListItems(t *testing.T) {
	items, err := ListItems([]byte(listDoc), "pools")
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %v", items, err)
	}
	if items[0].Name != "a" || items[0].Text != "name: a\nprotocol: tavily   # 搜索\nbase_url: \"https://api.tavily.com\"\n" {
		t.Fatalf("a = %q", items[0].Text)
	}
	if items[1].Text != "name: b\nprotocol: rest\nbase_url: \"https://x\"\n# b 的注释\nmax_attempts: 2\n" {
		t.Fatalf("b = %q", items[1].Text)
	}
	if m, _ := ListItems([]byte(listDoc), "models"); len(m) != 1 || m[0].Name != "s1" {
		t.Fatalf("models = %+v", m)
	}
}

func TestReplaceAppendRemove(t *testing.T) {
	out, err := ReplaceListItem([]byte(listDoc), "pools", "a", "name: a\nprotocol: tavily\nbase_url: \"https://new\"\n")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(listDoc, "  - name: a\n    protocol: tavily   # 搜索\n    base_url: \"https://api.tavily.com\"\n", "  - name: a\n    protocol: tavily\n    base_url: \"https://new\"\n", 1)
	if string(out) != want {
		t.Fatalf("replace:\n%s", out)
	}
	out, err = AppendListItem([]byte(listDoc), "pools", "name: c\nprotocol: rest\nbase_url: \"https://c\"\n")
	if err != nil {
		t.Fatal(err)
	}
	want = strings.Replace(listDoc, "    max_attempts: 2\n", "    max_attempts: 2\n  - name: c\n    protocol: rest\n    base_url: \"https://c\"\n", 1)
	if string(out) != want {
		t.Fatalf("append:\n%s", out)
	}
	out, err = RemoveListItem([]byte(listDoc), "pools", "b")
	if err != nil {
		t.Fatal(err)
	}
	want = strings.Replace(listDoc, "  - name: b\n    protocol: rest\n    base_url: \"https://x\"\n    # b 的注释\n    max_attempts: 2\n", "", 1)
	if string(out) != want {
		t.Fatalf("remove:\n%s", out)
	}
	if items, _ := ListItems(out, "pools"); len(items) != 1 {
		t.Fatalf("after remove: %+v", items)
	}
	out, _ = RemoveListItem([]byte(listDoc), "models", "s1")
	if !strings.HasSuffix(string(out), "# 服务\nmodels:\n") {
		t.Fatalf("remove last model: %q", out)
	}
	for _, f := range []func() error{
		func() error { _, err := ReplaceListItem([]byte(listDoc), "pools", "zz", "name: zz\n"); return err },
		func() error { _, err := RemoveListItem([]byte(listDoc), "models", "zz"); return err },
	} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("missing item: %v", err)
		}
	}
}

func TestAppendCreatesOrFillsList(t *testing.T) {
	for in, want := range map[string]string{
		"server:\n  data_dir: x\n":        "server:\n  data_dir: x\nmodels:\n  - name: m\n    pool: p\n",
		"models: []\nserver: {}\n":        "models:\n  - name: m\n    pool: p\nserver: {}\n",
		"models:\nserver: {}":             "models:\n  - name: m\n    pool: p\nserver: {}\n",
		"models:\n- name: x\n  pool: p\n": "models:\n- name: x\n  pool: p\n- name: m\n  pool: p\n",
	} {
		out, err := AppendListItem([]byte(in), "models", "name: m\npool: p\n")
		if err != nil || string(out) != want {
			t.Errorf("%q:\n got %q (%v)\nwant %q", in, out, err, want)
		}
	}
	if _, err := AppendListItem([]byte("models: [{name: x}]\n"), "models", "name: m\n"); err == nil {
		t.Error("flow list accepted")
	}
}

func TestEntryName(t *testing.T) {
	if n, err := EntryName("name: x\nprotocol: rest\n"); err != nil || n != "x" {
		t.Fatalf("%q %v", n, err)
	}
	for _, bad := range []string{"- a\n", "protocol: rest\n", "name: [\n", ""} {
		if _, err := EntryName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEntryFields(t *testing.T) {
	text := "name: s1\npool: a # 池\nprice: { credits: \"0.008\" }\napi: { errors: [] }\n"
	f, err := ReadEntryFields("models", text)
	if err != nil || f["pool"] != "a" || f["price"].(map[string]string)["credits"] != "0.008" {
		t.Fatalf("%v %v", f, err)
	}
	out, err := ApplyEntryFields("models", text, EntryFields{"pool": "b", "upstream_model": "", "price": map[string]any{"credits": "0.01", "requests": ""}, "reserve": map[string]any{"credits": float64(2)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"pool: b # 池", `price: {credits: "0.01"}`, "api: {errors: []}", "reserve: {credits: 2}"} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "upstream_model") || strings.Contains(out, "requests") {
		t.Errorf("empty values kept:\n%s", out)
	}
	out, err = ApplyEntryFields("pools", "", EntryFields{"name": "p", "protocol": "rest", "base_url": "https://x", "max_attempts": float64(3), "strategy": ""})
	if err != nil || out != "name: p\nprotocol: rest\nbase_url: https://x\nmax_attempts: 3\n" {
		t.Fatalf("new pool = %q %v", out, err)
	}
	if f, _ := ReadEntryFields("pools", out); f["max_attempts"] != 3 || f["base_url"] != "https://x" {
		t.Fatalf("read back = %v", f)
	}
	if _, err := ApplyEntryFields("x", "", nil); err == nil {
		t.Error("unknown kind accepted")
	}
}

// 单行流式写法的项（- { name: x, ... }）也能列出、替换与删除。
func TestFlowStyleItems(t *testing.T) {
	doc := "pools:\n  - { name: a, protocol: rest }\n  - { name: b, protocol: rest }\nmodels: []\n"
	items, err := ListItems([]byte(doc), "pools")
	if err != nil || len(items) != 2 || items[1].Text != "{ name: b, protocol: rest }\n" {
		t.Fatalf("%+v %v", items, err)
	}
	out, err := ReplaceListItem([]byte(doc), "pools", "a", "name: a\nprotocol: tavily\n")
	if err != nil || string(out) != "pools:\n  - name: a\n    protocol: tavily\n  - { name: b, protocol: rest }\nmodels: []\n" {
		t.Fatalf("replace = %q %v", out, err)
	}
	out, err = RemoveListItem([]byte(doc), "pools", "b")
	if err != nil || string(out) != "pools:\n  - { name: a, protocol: rest }\nmodels: []\n" {
		t.Fatalf("remove = %q %v", out, err)
	}
}
