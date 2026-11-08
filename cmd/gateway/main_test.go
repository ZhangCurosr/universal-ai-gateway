package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"version"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: code=%d out=%q", code, out.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestCheckConfig(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "gateway.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := realMain([]string{"check-config", "--config", path}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "config ok") {
		t.Fatalf("check-config: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte("pools: []\n"), 0o600)
	if code := realMain([]string{"check-config", "--config", bad}, &out, &errOut); code != 1 {
		t.Fatalf("check-config(bad): code = %d, want 1", code)
	}
}

// 全部已注册的适配器必须能通过注册表校验（REG-01 ~ REG-04）。
func TestRegistrySeals(t *testing.T) {
	reg, err := newRegistry()
	if err != nil {
		t.Fatalf("newRegistry() error = %v", err)
	}
	if reg.Fallback() == nil {
		t.Fatal("no fallback ingress")
	}
}

// CLI-05：客户端子命令只从环境变量读取连接信息
func TestClientCommandsRequireToken(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, args := range [][]string{{"tools"}, {"call", "ask_model", "--args", "{}"}, {"mcp"}} {
		var out, errb strings.Builder
		if code := realMainIO(args, strings.NewReader(""), &out, &errb, env(nil)); code != 2 || !strings.Contains(errb.String(), "UAIG_TOKEN") {
			t.Errorf("%v: code=%d stderr=%q", args, code, errb.String())
		}
	}
	var out, errb strings.Builder
	code := realMainIO([]string{"tools"}, strings.NewReader(""), &out, &errb, env(map[string]string{"UAIG_TOKEN": "uai_live_x", "UAIG_URL": "ftp://x"}))
	if code != 2 || !strings.Contains(errb.String(), "UAIG_URL") {
		t.Errorf("bad URL: code=%d stderr=%q", code, errb.String())
	}
	code = realMainIO([]string{"call", "t", "--args", "[1]"}, strings.NewReader(""), &out, &errb, env(map[string]string{"UAIG_TOKEN": "uai_live_x"}))
	if code != 2 {
		t.Errorf("non-object args: code=%d", code)
	}
	if code := realMainIO([]string{"call"}, strings.NewReader(""), &out, &errb, env(nil)); code != 2 {
		t.Errorf("call without tool name: code=%d", code)
	}
}
