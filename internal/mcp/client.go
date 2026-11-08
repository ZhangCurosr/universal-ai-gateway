package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// Client 是网关 MCP 端点的客户端（spec/14 §5），供 `gateway tools`、`gateway call`、`gateway mcp` 使用。
// 它只是 HTTP 客户端：不持有上游 Key，不访问数据库，可以在任何能访问网关的机器上运行。
type Client struct {
	URL   string // 网关 MCP 端点，如 http://127.0.0.1:8080/mcp
	Token string // 网关客户端 Token（uai_live_...）；不得写入日志或错误信息
	HTTP  *http.Client
}

// ToolInfo 是 tools/list 返回的一个工具。
type ToolInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// CallResult 是 tools/call 的结果。
type CallResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		Data     string `json:"data,omitempty"`
		MIMEType string `json:"mimeType,omitempty"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

// Post 发送一条 JSON-RPC 消息（CLI-01）：POST URL，头 `Authorization: Bearer <Token>`、
// `Content-Type: application/json`、`Accept: application/json, text/event-stream`、`MCP-Protocol-Version: SupportedVersions[0]`。
// 返回响应体；202 返回 nil；非 2xx 返回 *apperr.Error（消息取 JSON-RPC 错误的 message，不含 Token）。
func (c *Client) Post(ctx context.Context, msg []byte) ([]byte, error) {
	// TODO(T33)
	return nil, apperr.NotImplemented("T33")
}

// ListTools 调用 initialize 后再调用 tools/list（CLI-02）。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	// TODO(T33)
	return nil, apperr.NotImplemented("T33")
}

// CallTool 调用 initialize 后再调用 tools/call（CLI-03）。JSON-RPC 错误返回 error；工具执行错误在 CallResult.IsError 中。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	// TODO(T33)
	return nil, apperr.NotImplemented("T33")
}

// Bridge 实现 stdio 传输到 Streamable HTTP 的桥（CLI-04）：从 in 逐行读取 JSON-RPC 消息（每行一条，
// 单行上限 MaxMessageBytes），用 Post 转发，把非空响应作为一行写到 out；in 读到 EOF 或 ctx 取消时返回 nil。
// 单条消息转发失败时：若是请求，向 out 写一条该 id 的 JSON-RPC 错误（CodeInternalError）后继续；若是通知，忽略。
// 诊断信息写到 log（stderr），绝不写到 out（out 只能出现 JSON-RPC 报文）。
func Bridge(ctx context.Context, c *Client, in io.Reader, out io.Writer, log io.Writer) error {
	// TODO(T33)
	return apperr.NotImplemented("T33")
}
