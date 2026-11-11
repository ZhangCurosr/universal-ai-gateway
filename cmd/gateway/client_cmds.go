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
	for _, t := range tools {
		if t.Name == mcp.CatalogSearchTool {
			return listCatalogTools(ctx, c, *asJSON, stdout, stderr)
		}
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

// catalogPage 是 gateway_search_tools 结果中的 structuredContent（spec/14 CAT-04）。
type catalogPage struct {
	Tools []struct {
		Name     string `json:"name"`
		Title    string `json:"title,omitempty"`
		Summary  string `json:"summary"`
		ReadOnly bool   `json:"read_only"`
	} `json:"tools"`
	Total   int  `json:"total"`
	HasMore bool `json:"has_more"`
}

// listCatalogTools 实现 CLI-08：服务端处于目录模式时，翻页调用 gateway_search_tools 列出全部真实工具。
func listCatalogTools(ctx context.Context, c *mcp.Client, asJSON bool, stdout, stderr io.Writer) int {
	var all catalogPage
	for offset := 0; ; {
		args, _ := json.Marshal(map[string]int{"limit": 100, "offset": offset}) // 固定结构，必然可以序列化
		res, err := c.CallTool(ctx, mcp.CatalogSearchTool, args)
		if err != nil {
			fmt.Fprintln(stderr, "gateway:", err)
			return 1
		}
		var page catalogPage
		if res.IsError || json.Unmarshal(res.StructuredContent, &page) != nil {
			fmt.Fprintln(stderr, "gateway: unexpected tool catalog response")
			return 1
		}
		all.Tools = append(all.Tools, page.Tools...)
		offset += len(page.Tools)
		if !page.HasMore || len(page.Tools) == 0 {
			break
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(all.Tools, "", "  ") // 由 json.Unmarshal 得到的值必然可以序列化
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	for _, t := range all.Tools {
		fmt.Fprintf(stdout, "%-24s %s\n", t.Name, t.Summary)
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

// ---------------------------------------------------------------- gateway reload（spec/03 HR-02）

const (
	envAdminURL     = "UAIG_ADMIN_URL"   // 网关管理面地址，缺省 http://127.0.0.1:8081
	envAdminToken   = "UAIG_ADMIN_TOKEN" // 管理员 Token，必填
	reloadPollEvery = 500 * time.Millisecond
)

// cliReloadResponse 是 POST /admin/api/reload 的响应（ADM-18）。
type cliReloadResponse struct {
	Mode     string          `json:"mode"`
	Applied  bool            `json:"applied"`
	RecordID int64           `json:"record_id"`
	Changes  json.RawMessage `json:"changes"`
	Warnings []string        `json:"warnings"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	// 进程内重启完成后由 CLI 补上：最终状态与错误
	FinalStatus string `json:"final_status,omitempty"`
	FinalError  string `json:"final_error,omitempty"`
}

// cmdReload 实现 `gateway reload [--dry-run] [--json] [--timeout 2m]`（HR-02）。
func cmdReload(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "only report what would change")
	asJSON := fs.Bool("json", false, "print the full response as JSON")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up waiting for a restart after this long")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := getenv(envAdminToken)
	if token == "" {
		fmt.Fprintln(stderr, "gateway: "+envAdminToken+" is required")
		return 2
	}
	base := strings.TrimRight(getenv(envAdminURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8081"
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	client := newCLIHTTPClient()

	reqBody, _ := json.Marshal(map[string]bool{"dry_run": *dryRun}) // 固定结构，必然可以序列化
	status, raw, err := adminRequest(ctx, client, http.MethodPost, base+"/admin/api/reload", token, reqBody)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", redactToken(err.Error(), token))
		return 1
	}
	var resp cliReloadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		fmt.Fprintf(stderr, "gateway: unexpected response (HTTP %d)\n", status)
		return 1
	}
	if status >= 400 {
		msg := fmt.Sprintf("HTTP %d", status)
		if resp.Error != nil {
			msg = resp.Error.Message
		}
		fmt.Fprintln(stderr, "gateway: reload failed:", msg)
		return 1
	}
	code := 0
	if status == http.StatusAccepted && !*dryRun {
		final, errMsg, err := waitRestart(ctx, client, base, token, resp.RecordID)
		if err != nil {
			fmt.Fprintln(stderr, "gateway: waiting for restart:", redactToken(err.Error(), token))
			return 1
		}
		resp.FinalStatus, resp.FinalError = final, errMsg
		if final != reloadStatusApplied {
			code = 1
		}
	}
	if *asJSON {
		b, _ := json.MarshalIndent(resp, "", "  ") // 由 json.Unmarshal 得到的值必然可以序列化
		fmt.Fprintln(stdout, string(b))
		return code
	}
	printReloadSummary(stdout, &resp, *dryRun)
	if resp.FinalError != "" {
		fmt.Fprintln(stderr, "gateway: new configuration failed to start; rolled back:", resp.FinalError)
	}
	return code
}

// waitRestart 轮询 GET /admin/api/reload，直到记录 id 不再是 restarting。重启期间的连接错误视为「尚未就绪」。
func waitRestart(ctx context.Context, client *http.Client, base, token string, id int64) (string, string, error) {
	t := time.NewTicker(reloadPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-t.C:
		}
		status, raw, err := adminRequest(ctx, client, http.MethodGet, base+"/admin/api/reload", token, nil)
		if err != nil || status != http.StatusOK {
			continue
		}
		var out struct {
			Records []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"records"`
		}
		if json.Unmarshal(raw, &out) != nil {
			continue
		}
		for _, r := range out.Records {
			if r.ID == id && r.Status != reloadStatusRestarting {
				return r.Status, r.Error, nil
			}
		}
	}
}

// adminRequest 发送一个管理 API 请求，返回状态码与响应体（上限 1 MiB）。
func adminRequest(ctx context.Context, client *http.Client, method, url, token string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // 只读响应体，关闭失败无可处理
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<redacted>")
}

// printReloadSummary 以人类可读的形式输出重载结果。
func printReloadSummary(w io.Writer, r *cliReloadResponse, dryRun bool) {
	state := "applied"
	switch {
	case dryRun:
		state = "dry run, nothing applied"
	case r.Mode == reloadModeNone:
		state = "no changes"
	case r.FinalStatus != "":
		state = r.FinalStatus
	}
	fmt.Fprintf(w, "mode: %s (%s)\n", r.Mode, state)
	var ch reloadChanges
	if json.Unmarshal(r.Changes, &ch) == nil {
		for _, l := range []struct {
			label string
			names []string
		}{
			{"pools added", ch.Pools.Added}, {"pools removed", ch.Pools.Removed}, {"pools changed", ch.Pools.Changed},
			{"services added", ch.Services.Added}, {"services removed", ch.Services.Removed}, {"services changed", ch.Services.Changed},
			{"settings changed", ch.Settings}, {"needs restart", ch.RestartFields},
		} {
			if len(l.names) > 0 {
				fmt.Fprintf(w, "%s: %s\n", l.label, strings.Join(l.names, ", "))
			}
		}
	}
	for _, warn := range r.Warnings {
		fmt.Fprintln(w, "warning:", warn)
	}
}

// cmdAdminMCP 实现 `gateway admin-mcp`（CLI-10）：stdio 上的 MCP 传输，桥接到管理面的 /admin/mcp。
func cmdAdminMCP(ctx context.Context, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	token := getenv(envAdminToken)
	if token == "" {
		fmt.Fprintln(stderr, "gateway: "+envAdminToken+" is required")
		return 2
	}
	base := strings.TrimRight(getenv(envAdminURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8081"
	}
	c := &mcp.Client{URL: base + "/admin/mcp", Token: token, HTTP: newCLIHTTPClient()}
	if err := mcp.Bridge(ctx, c, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "gateway:", redactToken(err.Error(), token))
		return 1
	}
	return 0
}
