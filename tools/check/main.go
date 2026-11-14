// Command check 依次执行全部质量检查（spec/02 §1），任何一步失败即以非零码退出。
//
//	go run ./tools/check          完整检查
//	go run ./tools/check -short   跳过 -race（本机无 C 编译器时自动跳过）
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type step struct {
	name string
	args []string
	env  []string
	// check 对输出做额外判断；返回非空字符串表示失败原因。
	check func(out string) string
}

func main() {
	short := flag.Bool("short", false, "skip the race detector")
	flag.Parse()

	steps := []step{
		{name: "gofmt", args: []string{"gofmt", "-l", "cmd", "internal", "tools"}, check: func(out string) string {
			if strings.TrimSpace(out) != "" {
				return "these files need gofmt -w:\n" + out
			}
			return ""
		}},
		{name: "go vet", args: []string{"go", "vet", "./..."}},
		{name: "go build", args: []string{"go", "build", "./..."}},
		{name: "zero-CGO build", args: []string{"go", "build", "-o", os.DevNull, "./cmd/gateway"}, env: []string{"CGO_ENABLED=0"}},
		// 技能：.claude/skills 必须是 .agents/skills 的副本（tools/skillsync）
		{name: "skills in sync", args: []string{"go", "run", "./tools/skillsync", "-check"}},
	}
	testArgs := []string{"go", "test", "-count=1", "./..."}
	race := !*short && raceSupported()
	if race {
		testArgs = []string{"go", "test", "-count=1", "-race", "./..."}
	}
	steps = append(steps, step{name: "go test", args: testArgs, env: raceEnv(race)})

	for _, s := range steps {
		fmt.Printf("==> %s\n", strings.Join(s.args, " "))
		cmd := exec.Command(s.args[0], s.args[1:]...)
		cmd.Env = append(os.Environ(), s.env...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		out := buf.String()
		if err != nil {
			fmt.Print(out)
			fmt.Printf("FAIL: %s: %v\n", s.name, err)
			os.Exit(1)
		}
		if s.check != nil {
			if msg := s.check(out); msg != "" {
				fmt.Printf("FAIL: %s: %s\n", s.name, msg)
				os.Exit(1)
			}
		}
		if s.name == "go test" {
			fmt.Print(summarize(out))
		}
	}
	if !race {
		fmt.Println("note: race detector skipped (needs CGO_ENABLED=1 and a C compiler, or -short was given)")
	}
	fmt.Println("ALL CHECKS PASSED")
}

// raceSupported 报告本机能否使用 -race（需要 cgo 与 C 编译器）。
func raceSupported() bool {
	out, err := exec.Command("go", "env", "CC").Output()
	if err != nil {
		return false
	}
	cc := strings.TrimSpace(string(out))
	if cc == "" {
		return false
	}
	_, err = exec.LookPath(cc)
	return err == nil
}

func raceEnv(race bool) []string {
	if race {
		return []string{"CGO_ENABLED=1"}
	}
	return nil
}

// summarize 只保留每个包的结果行，并统计被 Skip 的 TODO 测试数量。
func summarize(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ok") || strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "?") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
