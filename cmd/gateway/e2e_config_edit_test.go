package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// T72：网页端管理池与服务（PE-01 ~ PE-05）。新建、检查、版本冲突、改名、删除；每一步之后文件只有对应的几行变化，
// 全部删除后文件与最初完全相同。
func TestE2E_ConfigEntries(t *testing.T) {
	url := restUpstream(t, nil, nil)
	spec := gwSpec{pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0")}
	g := startReloadGateway(t, spec)
	orig, err := os.ReadFile(g.path)
	if err != nil {
		t.Fatal(err)
	}
	type entries struct {
		Version  string `json:"version"`
		Writable bool   `json:"writable"`
		Pools    []struct {
			Name, YAML, Protocol string
			Services, Keys       int
		} `json:"pools"`
		Models []struct {
			Name, YAML, Pool string
		} `json:"models"`
		Protocols []struct {
			Name           string   `json:"name"`
			Units          []string `json:"units"`
			DefaultBaseURL string   `json:"default_base_url"`
		} `json:"protocols"`
	}
	load := func() entries {
		var e entries
		if code := doJSON(t, "GET", g.adminURL()+"/config/entries", g.adminToken, nil, &e); code != 200 {
			t.Fatalf("entries: %d", code)
		}
		return e
	}
	e := load()
	if !e.Writable || len(e.Pools) != 1 || e.Pools[0].Name != "p1" || e.Pools[0].Protocol != "rest" || e.Pools[0].Services != 1 || len(e.Models) != 1 || e.Models[0].Pool != "p1" {
		t.Fatalf("entries = %+v", e)
	}
	var tavily bool
	for _, p := range e.Protocols {
		if p.Name == "tavily" && strings.Join(p.Units, ",") == "credits" && p.DefaultBaseURL == "https://api.tavily.com" {
			tavily = true
		}
	}
	if !tavily {
		t.Fatalf("protocols = %+v", e.Protocols)
	}
	send := func(method, path string, body map[string]any) (int, map[string]any) {
		var out map[string]any
		code := doJSON(t, method, g.adminURL()+path, g.adminToken, body, &out)
		return code, out
	}
	pool2 := fmt.Sprintf("name: p2\nprotocol: rest\nbase_url: %q\n", url)
	// check：只校验，不写文件
	if code, out := send("POST", "/config/pools", map[string]any{"version": e.Version, "yaml": pool2, "check": true}); code != 200 || out["ok"] != true {
		t.Fatalf("check: %d %v", code, out)
	}
	if now, _ := os.ReadFile(g.path); string(now) != string(orig) {
		t.Fatal("check wrote the file")
	}
	code, out := send("POST", "/config/pools", map[string]any{"version": e.Version, "yaml": pool2})
	if code != 200 || out["version"] == e.Version {
		t.Fatalf("create pool: %d %v", code, out)
	}
	if code, _ := send("POST", "/config/pools", map[string]any{"version": e.Version, "yaml": pool2}); code != http.StatusConflict {
		t.Fatalf("stale version: %d", code)
	}
	e = load()
	if code, out := send("POST", "/config/pools", map[string]any{"version": e.Version, "yaml": pool2}); code != 400 || !strings.Contains(fmt.Sprint(out), "already exists") {
		t.Fatalf("duplicate: %d %v", code, out)
	}
	// 缺少价目单位 → 400，文件不变
	before, _ := os.ReadFile(g.path)
	if code, out := send("POST", "/config/models", map[string]any{"version": e.Version, "yaml": "name: s2\npool: p2\n"}); code != 400 || !strings.Contains(fmt.Sprint(out), "price") {
		t.Fatalf("missing price: %d %v", code, out)
	}
	if now, _ := os.ReadFile(g.path); string(now) != string(before) {
		t.Fatal("invalid edit changed the file")
	}
	if code, out := send("POST", "/config/models", map[string]any{"version": e.Version, "yaml": "name: s2\npool: p2\nprice: { requests: \"0\" }\n"}); code != 200 {
		t.Fatalf("create service: %d %v", code, out)
	}
	g.addKey("p2", "k2")
	token := g.createClient("u")
	if code, body := g.get(token, "/rest/s2/x"); code != 200 {
		t.Fatalf("new service: %d %s", code, body)
	}
	e = load()
	if code, out := send("PUT", "/config/pools/p2", map[string]any{"version": e.Version, "yaml": strings.Replace(pool2, "p2", "p3", 1)}); code != 400 || !strings.Contains(fmt.Sprint(out), "renamed") {
		t.Fatalf("rename: %d %v", code, out)
	}
	if code, out := send("PUT", "/config/pools/p2", map[string]any{"version": e.Version, "yaml": pool2 + "max_attempts: 2\n"}); code != 200 {
		t.Fatalf("update: %d %v", code, out)
	}
	e = load()
	if code, out := send("DELETE", "/config/pools/p2", map[string]any{"version": e.Version}); code != 400 || !strings.Contains(fmt.Sprint(out), "still has services") {
		t.Fatalf("delete pool with services: %d %v", code, out)
	}
	if code, out := send("DELETE", "/config/models/s2", map[string]any{"version": e.Version}); code != 200 {
		t.Fatalf("delete service: %d %v", code, out)
	}
	if code, _ := g.get(token, "/rest/s2/x"); code == 200 {
		t.Fatal("deleted service still answers")
	}
	e = load()
	if code, out := send("DELETE", "/config/pools/p2", map[string]any{"version": e.Version}); code != 200 {
		t.Fatalf("delete pool: %d %v", code, out)
	}
	if now, _ := os.ReadFile(g.path); string(now) != string(orig) {
		t.Fatalf("file differs from the original after undoing every change:\n%s", now)
	}
	if !strings.Contains(g.stderr.String(), "config edited by admin") {
		t.Error("edit not logged (PE-06)")
	}
	// compose：表单字段写进 YAML
	var comp struct {
		YAML   string         `json:"yaml"`
		Fields map[string]any `json:"fields"`
	}
	doJSON(t, "POST", g.adminURL()+"/config/compose", g.adminToken, map[string]any{"kind": "models", "yaml": "name: s9\npool: p1 # x\n",
		"fields": map[string]any{"price": map[string]any{"requests": "0.01"}}}, &comp)
	if !strings.Contains(comp.YAML, "pool: p1 # x") || !strings.Contains(comp.YAML, `requests: "0.01"`) || comp.Fields["name"] != "s9" {
		t.Fatalf("compose = %+v", comp)
	}
}
