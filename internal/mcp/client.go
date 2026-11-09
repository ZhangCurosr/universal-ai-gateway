package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", SupportedVersions[0])

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errMsg := err.Error()
		if c.Token != "" {
			errMsg = strings.ReplaceAll(errMsg, c.Token, "[REDACTED]")
		}
		return nil, apperr.Wrap(apperr.CodeUpstreamUnavailable, errMsg, errors.New(errMsg))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		ct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "text/event-stream") {
			r := sse.NewReader(resp.Body)
			for {
				ev, err := r.Next()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return nil, apperr.Wrap(apperr.CodeUpstreamError, "read sse event", err)
				}
				trimmed := bytes.TrimSpace(ev.Data)
				if len(trimmed) > 0 {
					return trimmed, nil
				}
			}
			return nil, apperr.New(apperr.CodeUpstreamError, "empty event stream response")
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxMessageBytes+1))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeUpstreamError, "read response body", err)
		}
		if int64(len(body)) > MaxMessageBytes {
			return nil, apperr.New(apperr.CodeRequestTooLarge, "response body too large")
		}
		return body, nil
	}

	// 非 2xx 响应处理
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	errMsg := http.StatusText(resp.StatusCode)
	var rpcErr struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &rpcErr); err == nil {
		if rpcErr.Error != nil && rpcErr.Error.Message != "" {
			errMsg = rpcErr.Error.Message
		} else if rpcErr.Message != "" {
			errMsg = rpcErr.Message
		}
	} else if len(body) > 0 && len(body) < 1024 {
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" {
			errMsg = trimmed
		}
	}
	if c.Token != "" {
		errMsg = strings.ReplaceAll(errMsg, c.Token, "[REDACTED]")
	}
	return nil, apperr.New(codeFromStatus(resp.StatusCode), errMsg)
}

func codeFromStatus(status int) apperr.Code {
	switch status {
	case http.StatusBadRequest:
		return apperr.CodeInvalidRequest
	case http.StatusUnauthorized:
		return apperr.CodeUnauthenticated
	case http.StatusPaymentRequired:
		return apperr.CodeInsufficientQuota
	case http.StatusForbidden:
		return apperr.CodePermissionDenied
	case http.StatusNotFound:
		return apperr.CodeNotFound
	case http.StatusConflict:
		return apperr.CodeConflict
	case http.StatusRequestEntityTooLarge:
		return apperr.CodeRequestTooLarge
	case http.StatusTooManyRequests:
		return apperr.CodeRateLimited
	case http.StatusBadGateway:
		return apperr.CodeUpstreamError
	case http.StatusServiceUnavailable:
		return apperr.CodeUpstreamUnavailable
	case http.StatusGatewayTimeout:
		return apperr.CodeTimeout
	default:
		return apperr.CodeInternal
	}
}

func (c *Client) handshake(ctx context.Context) error {
	initReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": SupportedVersions[0],
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "gateway-client",
				"version": "1.0",
			},
		},
	})
	body, err := c.Post(ctx, initReq)
	if err != nil {
		return err
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "decode initialize response", err)
	}
	if resp.Error != nil {
		if c.Token != "" {
			resp.Error.Message = strings.ReplaceAll(resp.Error.Message, c.Token, "[REDACTED]")
		}
		return resp.Error
	}

	notify, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	_, err = c.Post(ctx, notify)
	return err
}

// ListTools 调用 initialize 后再调用 tools/list（CLI-02）。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	if err := c.handshake(ctx); err != nil {
		return nil, err
	}
	listReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	respBody, err := c.Post(ctx, listReq)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response
		Result struct {
			Tools []ToolInfo `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "decode tools/list response", err)
	}
	if resp.Error != nil {
		if c.Token != "" {
			resp.Error.Message = strings.ReplaceAll(resp.Error.Message, c.Token, "[REDACTED]")
		}
		return nil, resp.Error
	}
	if resp.Result.Tools == nil {
		return []ToolInfo{}, nil
	}
	return resp.Result.Tools, nil
}

// CallTool 调用 initialize 后再调用 tools/call（CLI-03）。JSON-RPC 错误返回 error；工具执行错误在 CallResult.IsError 中。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	if err := c.handshake(ctx); err != nil {
		return nil, err
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	callReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
	respBody, err := c.Post(ctx, callReq)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response
		Result *CallResult `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "decode tools/call response", err)
	}
	if resp.Error != nil {
		if c.Token != "" {
			resp.Error.Message = strings.ReplaceAll(resp.Error.Message, c.Token, "[REDACTED]")
		}
		return nil, resp.Error
	}
	if resp.Result == nil {
		return nil, apperr.New(apperr.CodeInternal, "mcp: empty result in tools/call response")
	}
	return resp.Result, nil
}

// Bridge 实现 stdio 传输到 Streamable HTTP 的桥（CLI-04）：从 in 逐行读取 JSON-RPC 消息（每行一条，
// 单行上限 MaxMessageBytes），用 Post 转发，把非空响应作为一行写到 out；in 读到 EOF 或 ctx 取消时返回 nil。
// 单条消息转发失败时：若是请求，向 out 写一条该 id 的 JSON-RPC 错误（CodeInternalError）后继续；若是通知，忽略。
// 超过上限的行被丢弃：记一条诊断，并回一条 id 为 null 的 CodeInvalidRequest 错误（无法可靠取得其 id），然后继续，
// 不让单条超长消息结束整个会话。
// 诊断信息写到 log（stderr），绝不写到 out（out 只能出现 JSON-RPC 报文）。
func Bridge(ctx context.Context, c *Client, in io.Reader, out io.Writer, log io.Writer) error {
	br := bufio.NewReaderSize(in, 64*1024)
	for {
		if ctx.Err() != nil {
			return nil
		}
		raw, tooLong, rerr := readLine(br, MaxMessageBytes)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			if ctx.Err() != nil {
				return nil
			}
			c.diag(log, "gateway bridge read: "+rerr.Error())
			return rerr
		}
		if tooLong {
			c.diag(log, fmt.Sprintf("gateway: dropped a message larger than %d bytes", MaxMessageBytes))
			if err := writeLine(out, ErrorResponse(json.RawMessage("null"), CodeInvalidRequest,
				fmt.Sprintf("message exceeds %d bytes", MaxMessageBytes))); err != nil {
				return err
			}
		} else if line := bytes.TrimSpace(raw); len(line) > 0 {
			if err := c.forward(ctx, line, out, log); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
	}
}

// forward 转发一条消息并写出响应；只有写 out 失败时返回错误。
func (c *Client) forward(ctx context.Context, line []byte, out, log io.Writer) error {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(line, &probe) // 只用于取 id；不是合法 JSON 时交给网关报错

	respBody, err := c.Post(ctx, line)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		c.diag(log, "gateway: "+err.Error())
		if len(probe.ID) == 0 || !validID(probe.ID) || string(probe.ID) == "null" {
			return nil // 通知：忽略
		}
		msg := err.Error()
		if ap, ok := apperr.As(err); ok {
			msg = ap.Message
		}
		return writeLine(out, ErrorResponse(probe.ID, CodeInternalError, c.redact(msg)))
	}
	if body := bytes.TrimSpace(respBody); len(body) > 0 {
		return writeRaw(out, body)
	}
	return nil
}

// readLine 读取一行（不含换行符）。行超过 max 字节时丢弃到行尾并返回 tooLong=true。
// 最后一行没有换行符时与 io.EOF 一起返回。
func readLine(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max+1 { // +1：允许结尾的换行符
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, true, err
		}
		return bytes.TrimRight(buf, "\r\n"), false, err
	}
}

func (c *Client) redact(s string) string {
	if c.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Token, "[REDACTED]")
}

// diag 把诊断写到 log（脱敏 Token）；log 为 nil 时丢弃。
func (c *Client) diag(log io.Writer, msg string) {
	if log == nil {
		return
	}
	_, _ = fmt.Fprintf(log, "%s\n", c.redact(msg)) // 诊断写失败不影响桥接
	flush(log)
}

func writeLine(out io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json-rpc message: %w", err)
	}
	return writeRaw(out, b)
}

func writeRaw(out io.Writer, b []byte) error {
	if _, err := fmt.Fprintf(out, "%s\n", b); err != nil {
		return err
	}
	flush(out)
	return nil
}

func flush(w io.Writer) {
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush() // 尽力而为：后续写入会再次暴露错误
	}
}
