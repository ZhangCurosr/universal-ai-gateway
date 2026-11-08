// Package mcp 让 AI 通过 MCP（Model Context Protocol）与命令行调用网关接入的服务（spec/14，任务卡 T32、T33）。
//
//	server.go   MCP 服务端：Streamable HTTP 传输上的 JSON-RPC（initialize / ping / tools/list / tools/call）
//	invoke.go   把一次工具调用转换为对入站适配器调用端点的内部请求，交给内核处理
//	client.go   MCP 客户端：gateway tools / gateway call / gateway mcp（stdio 桥）使用
//	jsonrpc.go  JSON-RPC 2.0 报文（已实现）
//
// 本包不认识任何具体协议：工具来自注册表中实现了 protocol.ToolSource 的入站适配器。
// 工具调用经过内核，因此计费、选 Key、重试、白名单与普通 HTTP 请求完全相同。
// 本包不得导入 internal/adapter/*（ARCH-07），也不得访问数据库（ARCH-01）。
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
)

// JSON-RPC 2.0 错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Request 是一条 JSON-RPC 请求或通知（ID 为空表示通知）。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification 报告这条消息是否是通知（没有 id）。
func (r *Request) IsNotification() bool { return len(r.ID) == 0 }

// Response 是一条 JSON-RPC 响应。Result 与 Error 恰好一个非空。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error 是 JSON-RPC 错误对象。
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// errNotRequest 表示消息是一条 JSON-RPC 响应（客户端发来的响应，例如对服务端请求的回复）。本服务端不发起请求，忽略它们。
var errNotRequest = errors.New("mcp: message is a response, not a request")

// DecodeRequest 解析一条 JSON-RPC 消息（RPC-01 ~ RPC-03）：
//   - 不是合法 JSON → CodeParseError；
//   - 是数组（批量）→ CodeInvalidRequest（MCP 2025-06-18 已取消批量）；
//   - jsonrpc 不是 "2.0"、method 为空、id 不是字符串或数字 → CodeInvalidRequest；
//   - 是响应（有 result 或 error、没有 method）→ errNotRequest。
func DecodeRequest(body []byte) (*Request, error) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return nil, &Error{Code: CodeParseError, Message: "parse error"}
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return nil, &Error{Code: CodeInvalidRequest, Message: "batch requests are not supported"}
	}
	var probe struct {
		Request
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request"}
	}
	if probe.Method == "" && (probe.Result != nil || probe.Error != nil) {
		return nil, errNotRequest
	}
	req := probe.Request
	if req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) {
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request"}
	}
	return &req, nil
}

// validID：id 缺省（通知）或为字符串 / 数字；null 不允许（MCP 要求请求 id 非 null）。
func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	switch id[0] {
	case '"':
		var s string
		return json.Unmarshal(id, &s) == nil
	case 'n', 't', 'f', '{', '[':
		return false
	default:
		var n json.Number
		return json.Unmarshal(id, &n) == nil
	}
}

// ResultResponse 构造成功响应。result 序列化失败视为编程错误，返回 CodeInternalError。
func ResultResponse(id json.RawMessage, result any) *Response {
	b, err := json.Marshal(result)
	if err != nil {
		return ErrorResponse(id, CodeInternalError, "internal error")
	}
	return &Response{JSONRPC: "2.0", ID: id, Result: b}
}

// ErrorResponse 构造错误响应；id 为空时写 null。
func ErrorResponse(id json.RawMessage, code int, msg string) *Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg}}
}
