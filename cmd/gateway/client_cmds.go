package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

// 客户端子命令（spec/14 §5）：在任何能访问网关的机器上运行，不需要配置文件、不持有上游 Key。
//
//	gateway tools                         列出当前 Token 可用的工具
//	gateway call <tool> [--args JSON|-]   调用一个工具，结果文本写到 stdout
//	gateway mcp                           stdio MCP 服务端（桥接到网关的 /mcp），供 Claude Code 等本地启动
//
// 连接信息只从环境变量读取（CLI-05）：Token 放在命令行参数里会出现在进程列表与 shell 历史中。
const (
	envURL   = "UAIG_URL"   // 网关数据面地址，缺省 http://127.0.0.1:8080
	envToken = "UAIG_TOKEN" // 客户端 Token（uai_live_...），必填
)

// clientEnv 读取环境变量并构造 MCP 客户端；getenv 便于测试注入。
func clientEnv(getenv func(string) string, stderr io.Writer) (*mcp.Client, bool) {
	token := strings.TrimSpace(getenv(envToken))
	if token == "" {
		fmt.Fprintf(stderr, "gateway: %s is not set (create a client token with the admin API)\n", envToken)
		return nil, false
	}
	base := strings.TrimRight(strings.TrimSpace(getenv(envURL)), "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		fmt.Fprintf(stderr, "gateway: %s must start with http:// or https://\n", envURL)
		return nil, false
	}
	return &mcp.Client{URL: base + "/mcp", Token: token, HTTP: newCLIHTTPClient()}, true
}

// cmdTools 实现 `gateway tools [--json]`（CLI-06）。
func cmdTools(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the raw tool list as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, ok := clientEnv(getenv, stderr)
	if !ok {
		return 2
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(tools, "", "  ") // 由 json.Unmarshal 得到的值必然可以序列化
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	for _, t := range tools {
		fmt.Fprintf(stdout, "%-24s %s\n", t.Name, firstLine(t.Description))
	}
	return 0
}

// cmdCall 实现 `gateway call <tool> [--args JSON|-] [--json] [--timeout 10m]`（CLI-07）：
// 结果中的文本内容按顺序写到 stdout；--json 时输出完整结果；工具返回 isError 时退出码 1。
func cmdCall(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(stderr)
	argJSON := fs.String("args", "{}", `tool arguments as a JSON object, or "-" to read them from stdin`)
	asJSON := fs.Bool("json", false, "print the full tool result as JSON")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up after this long")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: gateway call <tool> [--args JSON|-] [--json] [--timeout 10m]")
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
	c, ok := clientEnv(getenv, stderr)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	res, err := c.CallTool(ctx, name, raw)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ") // 同上
		fmt.Fprintln(stdout, string(b))
	} else {
		for _, part := range res.Content {
			if part.Type == "text" {
				fmt.Fprintln(stdout, part.Text)
			} else {
				fmt.Fprintf(stderr, "gateway: [%s content omitted; use --json]\n", part.Type)
			}
		}
	}
	if res.IsError {
		return 1
	}
	return 0
}

// cmdMCP 实现 `gateway mcp`（CLI-04）：stdin/stdout 上的 MCP stdio 传输，桥接到网关的 /mcp。
func cmdMCP(ctx context.Context, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	c, ok := clientEnv(getenv, stderr)
	if !ok {
		return 2
	}
	if err := mcp.Bridge(ctx, c, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return 1
	}
	return 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// newCLIHTTPClient 返回 CLI 用的 HTTP 客户端：不设 Client.Timeout（同步封装的工具调用可能
// 持续数分钟，总时长由网关侧的超时控制），但连接与 TLS 握手必须有上限，否则网关地址
// 不可达时 `gateway tools` / `gateway mcp` 会无限挂起（AGENTS §3 #6）。
func newCLIHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}
