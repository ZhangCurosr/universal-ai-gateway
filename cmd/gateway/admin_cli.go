package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

// adminToolPrefix 是管理面 MCP 工具名的公共前缀（AM-02）；命令行可以省略它（AC-02）。
const adminToolPrefix = "gateway_admin_"

// adminClient 读取 UAIG_ADMIN_URL / UAIG_ADMIN_TOKEN，构造连到 /admin/mcp 的客户端（与 gateway admin-mcp 相同）。
func adminClient(getenv func(string) string, stderr io.Writer) (*mcp.Client, bool) {
	token := strings.TrimSpace(getenv(envAdminToken))
	if token == "" {
		fmt.Fprintln(stderr, "gateway: "+envAdminToken+" is required")
		return nil, false
	}
	base := strings.TrimRight(strings.TrimSpace(getenv(envAdminURL)), "/")
	if base == "" {
		base = "http://127.0.0.1:8081"
	}
	return &mcp.Client{URL: base + "/admin/mcp", Token: token, HTTP: newCLIHTTPClient()}, true
}

// cmdAdmin 实现 `gateway admin tools [--json]` 与 `gateway admin call <工具> [--args JSON|-] [--json] [--yes]`（T74，AC-01 ~ AC-05）。
func cmdAdmin(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "tools" && args[0] != "call") {
		fmt.Fprintln(stderr, "usage: gateway admin tools [--json] | gateway admin call <tool> [--args JSON|-] [--json] [--yes]")
		return 2
	}
	if args[0] == "tools" {
		return adminToolsCmd(ctx, args[1:], getenv, stdout, stderr)
	}
	return adminCallCmd(ctx, args[1:], getenv, stdin, stdout, stderr)
}

func shortToolName(name string) string { return strings.TrimPrefix(name, adminToolPrefix) }

// adminToolsCmd 实现 AC-01。
func adminToolsCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin tools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the raw tool list as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, ok := adminClient(getenv, stderr)
	if !ok {
		return 2
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", redactToken(err.Error(), c.Token))
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(tools, "", "  ") // 由 json.Unmarshal 得到的值必然可以序列化
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	for _, t := range tools {
		kind := "write"
		if t.ReadOnly() {
			kind = "read"
		}
		fmt.Fprintf(stdout, "%-16s %-5s  %s\n", shortToolName(t.Name), kind, firstLine(t.Description))
	}
	return 0
}

// adminCallCmd 实现 AC-02 ~ AC-05。
func adminCallCmd(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin call", flag.ContinueOnError)
	fs.SetOutput(stderr)
	argJSON := fs.String("args", "{}", `tool arguments as a JSON object, or "-" to read them from stdin`)
	asJSON := fs.Bool("json", false, "print the full tool result as JSON")
	yes := fs.Bool("yes", false, "confirm a tool that changes the gateway")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: gateway admin call <tool> [--args JSON|-] [--json] [--yes]")
		return 2
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	raw := []byte(*argJSON)
	if *argJSON == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, mcp.MaxMessageBytes))
		if err != nil {
			fmt.Fprintln(stderr, "gateway: read stdin:", err)
			return 1
		}
		raw = b
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		fmt.Fprintln(stderr, "gateway: --args must be a JSON object")
		return 2
	}
	c, ok := adminClient(getenv, stderr)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tools, err := c.ListTools(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", redactToken(err.Error(), c.Token))
		return 1
	}
	var tool *mcp.ToolInfo
	var names []string
	for i := range tools {
		names = append(names, shortToolName(tools[i].Name))
		if tools[i].Name == name || tools[i].Name == adminToolPrefix+name {
			tool = &tools[i]
		}
	}
	if tool == nil { // AC-05
		sort.Strings(names)
		fmt.Fprintf(stderr, "gateway: unknown admin tool %q; available: %s\n", name, strings.Join(names, ", "))
		return 2
	}
	// AC-03：写入类工具需要 --yes；reload 的 dry-run 视为只读
	dryRun := string(obj["dry_run"]) == "true"
	if !tool.ReadOnly() && !(shortToolName(tool.Name) == "reload" && dryRun) && !*yes {
		fmt.Fprintf(stderr, "gateway: this changes the gateway: %s %s\nrun again with --yes to confirm\n", shortToolName(tool.Name), strings.TrimSpace(string(raw)))
		return 2
	}
	res, err := c.CallTool(ctx, tool.Name, raw)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", redactToken(err.Error(), c.Token))
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ") // 同上
		fmt.Fprintln(stdout, string(b))
	} else {
		out := stdout
		if res.IsError {
			out = stderr
		}
		for _, part := range res.Content {
			if part.Type == "text" {
				fmt.Fprintln(out, redactToken(part.Text, c.Token))
			}
		}
	}
	if res.IsError {
		return 1
	}
	return 0
}
