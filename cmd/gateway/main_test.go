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
