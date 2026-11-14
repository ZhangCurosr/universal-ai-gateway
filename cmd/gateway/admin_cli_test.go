package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// T74：gateway admin tools / call（AC-01 ~ AC-05）对真实管理面。
func TestAdminCLI(t *testing.T) {
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "ac", Protocol: "rest", BaseURL: "http://127.0.0.1:1"})
		c.Models = append(c.Models, config.ModelConfig{Name: "ac-api", Pool: "ac", Price: map[string]string{"requests": "0"}})
		c.Normalize()
	})
	doJSON(t, "POST", "http://"+g.admin+"/admin/api/keys", g.adminToken, map[string]any{"pool": "ac", "name": "k1", "secret": "ac-secret-1111"}, nil)
	env := func(k string) string {
		return map[string]string{envAdminURL: "http://" + g.admin, envAdminToken: g.adminToken}[k]
	}
	run := func(args ...string) (int, string, string) {
		var out, errOut strings.Builder
		code := cmdAdmin(context.Background(), args, env, strings.NewReader(""), &out, &errOut)
		if strings.Contains(out.String()+errOut.String(), g.adminToken) {
			t.Fatalf("%v: admin token in output", args)
		}
		return code, out.String(), errOut.String()
	}

	code, out, _ := run("tools")
	if code != 0 || !strings.Contains(out, "pools") || !strings.Contains(out, "read") || !strings.Contains(out, "set_key_state") || strings.Contains("\n"+out, "\ngateway_admin_") {
		t.Fatalf("tools: %d %q", code, out)
	}
	if code, out, _ := run("tools", "--json"); code != 0 || !strings.Contains(out, `"gateway_admin_pools"`) || !strings.Contains(out, `"readOnlyHint": true`) {
		t.Fatalf("tools --json: %d %q", code, out)
	}
	// 短名与全名
	for _, name := range []string{"pools", "gateway_admin_pools"} {
		if code, out, _ := run("call", name); code != 0 || !strings.Contains(out, `"pool":"ac"`) {
			t.Fatalf("call %s: %d %q", name, code, out)
		}
	}
	// 写入类需要 --yes
	code, out, errOut := run("call", "set_key_state", "--args", `{"id":1,"state":"disabled"}`)
	if code != 2 || out != "" || !strings.Contains(errOut, "--yes") {
		t.Fatalf("without --yes: %d %q %q", code, out, errOut)
	}
	if code, _, _ := run("call", "list_keys"); code != 0 {
		t.Fatal("list_keys failed")
	}
	if code, out, errOut := run("call", "set_key_state", "--args", `{"id":1,"state":"disabled"}`, "--yes"); code != 0 {
		t.Fatalf("with --yes: %d %q %q", code, out, errOut)
	}
	var keys struct {
		Keys []struct{ State string } `json:"keys"`
	}
	doJSON(t, "GET", "http://"+g.admin+"/admin/api/keys", g.adminToken, nil, &keys)
	if len(keys.Keys) != 1 || keys.Keys[0].State != "disabled" {
		t.Fatalf("key state = %+v", keys)
	}
	// reload 的 dry-run 不需要 --yes（这个网关没有配置文件，工具返回错误 → 退出码 1，而不是 2）
	if code, _, errOut := run("call", "reload", "--args", `{"dry_run":true}`); code != 1 || strings.Contains(errOut, "--yes") {
		t.Fatalf("reload dry-run: %d %q", code, errOut)
	}
	if code, _, errOut := run("call", "reload"); code != 2 || !strings.Contains(errOut, "--yes") {
		t.Fatalf("reload without --yes: %d %q", code, errOut)
	}
	// 未知工具、参数错误、用法
	if code, _, errOut := run("call", "nope"); code != 2 || !strings.Contains(errOut, "available: ") || !strings.Contains(errOut, "pools") {
		t.Fatalf("unknown tool: %d %q", code, errOut)
	}
	if code, _, _ := run("call", "pools", "--args", "[1]"); code != 2 {
		t.Fatal("non-object args accepted")
	}
	for _, args := range [][]string{{}, {"bogus"}, {"call"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	var errB strings.Builder
	if code := cmdAdmin(context.Background(), []string{"tools"}, func(string) string { return "" }, strings.NewReader(""), &strings.Builder{}, &errB); code != 2 || !strings.Contains(errB.String(), envAdminToken) {
		t.Fatalf("missing token: %d %q", code, errB.String())
	}
}
