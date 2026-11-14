// Command skillsync 把 .agents/skills（Codex、OpenCode、Pi 读取的通用位置）同步到 .claude/skills（Claude Code 读取的位置）。
// Windows 上符号链接不可靠，所以用复制；tools/check 用 -check 确认两边一致。
//
//	go run ./tools/skillsync          同步（以 .agents/skills 为准，删除 .claude/skills 中多余的技能）
//	go run ./tools/skillsync -check   只检查，不一致时以非零码退出
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const (
	srcDir = ".agents/skills"
	dstDir = ".claude/skills"
)

func main() {
	check := flag.Bool("check", false, "only report differences")
	flag.Parse()
	diffs, err := sync(srcDir, dstDir, !*check)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skillsync:", err)
		os.Exit(1)
	}
	if *check && len(diffs) > 0 {
		fmt.Printf("%s and %s differ (run: go run ./tools/skillsync):\n", srcDir, dstDir)
		for _, d := range diffs {
			fmt.Println("  " + d)
		}
		os.Exit(1)
	}
	if !*check {
		fmt.Printf("synced %d change(s)\n", len(diffs))
	}
}

// listFiles 返回 root 下所有普通文件的相对路径（/ 分隔）→ 内容；root 不存在时返回空表。
func listFiles(root string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	return out, err
}

// sync 比较 src 与 dst，返回差异（按路径排序）；write 为 true 时让 dst 与 src 一致。
func sync(src, dst string, write bool) ([]string, error) {
	a, err := listFiles(src)
	if err != nil {
		return nil, err
	}
	b, err := listFiles(dst)
	if err != nil {
		return nil, err
	}
	var diffs []string
	for p, content := range a {
		old, ok := b[p]
		switch {
		case !ok:
			diffs = append(diffs, "missing "+p)
		case !bytes.Equal(old, content):
			diffs = append(diffs, "changed "+p)
		default:
			continue
		}
		if write {
			target := filepath.Join(dst, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(target, content, 0o644); err != nil {
				return nil, err
			}
		}
	}
	for p := range b {
		if _, ok := a[p]; ok {
			continue
		}
		diffs = append(diffs, "extra "+p)
		if write {
			if err := os.Remove(filepath.Join(dst, filepath.FromSlash(p))); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(diffs)
	return diffs, nil
}
