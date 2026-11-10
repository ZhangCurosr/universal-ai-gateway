// Package archtest 自动检查 spec/01 §4 的分层依赖规则。
//
// 本文件是架构约束，实现任务时禁止修改（AGENTS.md §3 第 10 条）。
// 需要新增包或调整依赖时，向架构负责人提出，由其修改本文件与 spec/01 §4。
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/ZhangCurosr/universal-ai-gateway/"

// layers 是每个包（相对仓库根目录的路径）所在的层。新增包必须在此登记。
var layers = map[string]int{
	"internal/apperr": 0,

	"internal/config":   1,
	"internal/obs":      1,
	"internal/crypto":   1,
	"internal/jsonx":    1,
	"internal/sse":      1,
	"internal/pricing":  1,
	"internal/store":    1,
	"internal/protocol": 1,
	"internal/ir":       1,
	"internal/clientip": 1,

	"internal/store/sqlite":        2,
	"internal/auth":                2,
	"internal/ratelimit":           2,
	"internal/ledger":              2,
	"internal/audit":               2,
	"internal/metrics":             2,
	"internal/scheduler":           2,
	"internal/adapter/anthropic":   2,
	"internal/adapter/gemini":      2,
	"internal/adapter/mineru":      2,
	"internal/adapter/openai":      2,
	"internal/adapter/openairesp":  2,
	"internal/adapter/rest":        2,
	"internal/adapter/tavily":      2,
	"internal/adapter/adaptertest": 2,
	"internal/translate":           2,

	"internal/core": 3,

	"internal/httpapi": 4,
	"internal/admin":   4,
	"internal/mcp":     4,

	"cmd/gateway": 5,

	"internal/archtest": 9, // 只有测试文件
}

// sameLayerAllowed 是明确允许的同层依赖。
var sameLayerAllowed = map[string][]string{
	"internal/store":    {"internal/crypto"},
	"internal/config":   {"internal/pricing"},
	"internal/protocol": {"internal/sse", "internal/pricing"},
	"internal/ir":       {"internal/pricing"},
}

// thirdParty 规定第三方模块只能被哪些包导入（spec/02 §3）。
var thirdParty = map[string][]string{
	"gopkg.in/yaml.v3":   {"internal/config"},
	"modernc.org/sqlite": {"internal/store/sqlite"},
}

// forbidden 是额外的禁止导入规则：包 → 不得导入的路径（含前缀匹配）。
var forbidden = map[string][]string{
	"internal/core":    {"database/sql", module + "internal/store/sqlite"}, // ARCH-01
	"internal/httpapi": {"database/sql", module + "internal/store/sqlite"}, // ARCH-01
	"internal/admin":   {module + "internal/core"},                         // ARCH-06
}

// adapterPrefix 下的具体适配器只能被 cmd/gateway 导入（ARCH-07：内核与其他包不得依赖具体协议）。
// adaptertest 只能被测试文件导入（ARCH-08）。
const adapterPrefix = module + "internal/adapter/"

type importInfo struct {
	file string
	path string
	test bool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found at %s", root)
	}
	return root
}

// collect 返回 包相对路径 → 导入列表。
func collect(t *testing.T, root string) map[string][]importInfo {
	t.Helper()
	out := map[string][]importInfo{}
	fset := token.NewFileSet()
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			if _, ok := out[rel]; !ok {
				out[rel] = nil
			}
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				out[rel] = append(out[rel], importInfo{file: filepath.Base(path), path: p, test: strings.HasSuffix(path, "_test.go")})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestLayering(t *testing.T) {
	root := repoRoot(t)
	pkgs := collect(t, root)
	names := make([]string, 0, len(pkgs))
	for p := range pkgs {
		names = append(names, p)
	}
	sort.Strings(names)

	for _, pkg := range names {
		from, ok := layers[pkg]
		if !ok {
			t.Errorf("package %s is not registered in archtest layers (new packages need architect approval)", pkg)
			continue
		}
		for _, imp := range pkgs[pkg] {
			if imp.test {
				continue // 测试文件不受分层约束（ARCH-05 另行检查）
			}
			checkForbidden(t, pkg, imp)
			checkAdapterIsolation(t, pkg, imp)
			checkThirdParty(t, pkg, imp)
			if !strings.HasPrefix(imp.path, module) {
				continue
			}
			target := strings.TrimPrefix(imp.path, module)
			to, ok := layers[target]
			if !ok {
				t.Errorf("%s/%s imports unregistered package %s", pkg, imp.file, target)
				continue
			}
			if to < from {
				continue
			}
			if to == from && contains(sameLayerAllowed[pkg], target) {
				continue
			}
			t.Errorf("ARCH: %s (layer %d) must not import %s (layer %d) [%s]", pkg, from, target, to, imp.file)
		}
	}
}

func checkAdapterIsolation(t *testing.T, pkg string, imp importInfo) {
	if !strings.HasPrefix(imp.path, adapterPrefix) {
		return
	}
	target := strings.TrimPrefix(imp.path, module)
	if target == "internal/adapter/adaptertest" {
		t.Errorf("ARCH-08: %s/%s imports adaptertest outside a _test.go file", pkg, imp.file)
		return
	}
	if pkg != "cmd/gateway" {
		t.Errorf("ARCH-07: %s/%s imports concrete adapter %s; only cmd/gateway/adapters.go may", pkg, imp.file, target)
	}
}

func checkForbidden(t *testing.T, pkg string, imp importInfo) {
	for _, bad := range forbidden[pkg] {
		if imp.path == bad || strings.HasPrefix(imp.path, bad+"/") {
			t.Errorf("ARCH: %s/%s must not import %s", pkg, imp.file, imp.path)
		}
	}
}

func checkThirdParty(t *testing.T, pkg string, imp importInfo) {
	first := strings.SplitN(imp.path, "/", 2)[0]
	if !strings.Contains(first, ".") || strings.HasPrefix(imp.path, module) {
		return // 标准库或本模块
	}
	for mod, allowed := range thirdParty {
		if imp.path == mod || strings.HasPrefix(imp.path, mod+"/") {
			if !contains(allowed, pkg) {
				t.Errorf("DEPS: %s/%s imports %s; only %v may", pkg, imp.file, imp.path, allowed)
			}
			return
		}
	}
	t.Errorf("DEPS: %s/%s imports non-whitelisted module %s (spec/02 §3)", pkg, imp.file, imp.path)
}

// ARCH-05
func TestNoMathRand(t *testing.T) {
	root := repoRoot(t)
	for pkg, imps := range collect(t, root) {
		for _, imp := range imps {
			if imp.path == "math/rand" {
				t.Errorf("ARCH-05: %s/%s imports math/rand", pkg, imp.file)
			}
			if imp.path == "math/rand/v2" && !imp.test {
				t.Errorf("ARCH-05: %s/%s imports math/rand/v2 outside tests", pkg, imp.file)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
