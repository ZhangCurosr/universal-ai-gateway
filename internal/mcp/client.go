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
// 诊断信息写到 log（stderr），绝不写到 out（out 只能出现 JSON-RPC 报文）。
func Bridge(ctx context.Context, c *Client, in io.Reader, out io.Writer, log io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), MaxMessageBytes)
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(line, &probe)

		respBody, err := c.Post(ctx, line)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if log != nil {
				errMsg := err.Error()
				if c.Token != "" {
					errMsg = strings.ReplaceAll(errMsg, c.Token, "[REDACTED]")
				}
				fmt.Fprintf(log, "gateway: %s\n", errMsg)
				if f, ok := log.(interface{ Flush() error }); ok {
					_ = f.Flush()
				}
			}
			if len(probe.ID) > 0 && validID(probe.ID) && string(probe.ID) != "null" {
				errMsg := err.Error()
				if ap, ok := apperr.As(err); ok {
					errMsg = ap.Message
				}
				if c.Token != "" {
					errMsg = strings.ReplaceAll(errMsg, c.Token, "[REDACTED]")
				}
				errResp := ErrorResponse(probe.ID, CodeInternalError, errMsg)
				b, _ := json.Marshal(errResp)
				if _, werr := fmt.Fprintf(out, "%s\n", b); werr != nil {
					return werr
				}
				if f, ok := out.(interface{ Flush() error }); ok {
					_ = f.Flush()
				}
			}
			continue
		}

		if len(bytes.TrimSpace(respBody)) > 0 {
			trimmed := bytes.TrimRight(respBody, "\r\n")
			if _, werr := fmt.Fprintf(out, "%s\n", trimmed); werr != nil {
				return werr
			}
			if f, ok := out.(interface{ Flush() error }); ok {
				_ = f.Flush()
			}
		}
	}

	if err := sc.Err(); err != nil {
		if ctx.Err() != nil || errors.Is(err, io.EOF) {
			return nil
		}
		if log != nil {
			errMsg := err.Error()
			if c.Token != "" {
				errMsg = strings.ReplaceAll(errMsg, c.Token, "[REDACTED]")
			}
			fmt.Fprintf(log, "gateway bridge read: %s\n", errMsg)
		}
		return err
	}
	return nil
}
