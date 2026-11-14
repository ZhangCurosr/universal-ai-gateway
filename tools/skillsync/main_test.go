package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSync(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "c")
	write(t, filepath.Join(src, "x", "SKILL.md"), "x1")
	write(t, filepath.Join(src, "y", "SKILL.md"), "y")
	write(t, filepath.Join(src, "y", "tool.py"), "py")
	write(t, filepath.Join(dst, "x", "SKILL.md"), "old")
	write(t, filepath.Join(dst, "z", "SKILL.md"), "gone")

	diffs, err := sync(src, dst, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(diffs, ";"); got != "changed x/SKILL.md;extra z/SKILL.md;missing y/SKILL.md;missing y/tool.py" {
		t.Fatalf("diffs = %s", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "y")); !os.IsNotExist(err) {
		t.Fatal("check mode wrote files")
	}
	if _, err := sync(src, dst, true); err != nil {
		t.Fatal(err)
	}
	if diffs, _ := sync(src, dst, false); len(diffs) != 0 {
		t.Fatalf("after sync: %v", diffs)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "x", "SKILL.md")); string(b) != "x1" {
		t.Fatalf("content = %q", b)
	}
	// 目标目录不存在时也能比较
	if diffs, err := sync(src, filepath.Join(dir, "none"), false); err != nil || len(diffs) != 3 {
		t.Fatalf("missing dst: %v %v", diffs, err)
	}
}
