package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/core"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// SupportedVersions 是支持的 MCP 协议版本，按新到旧排列（SRV-M02）。
var SupportedVersions = []string{"2025-06-18", "2025-03-26"}

// MaxMessageBytes 是单条 JSON-RPC 请求的上限。
const MaxMessageBytes = 4 << 20

// Deps 是 MCP 服务端的依赖。
type Deps struct {
	Registry       *protocol.Registry // 必须已 Seal
	Core           *core.Core
	MaxResultBytes int64 // 单次工具调用的端点响应上限（config mcp.max_result_bytes）
	Version        string
	Logger         *slog.Logger
}

// Server 是 MCP 服务端（Streamable HTTP 传输，无状态：不签发 Mcp-Session-Id）。并发安全。
// 挂在数据面 /mcp 上，前面已经过 IPRateLimit、Overload、Authenticate、RateLimit（spec/09 §1）。
type Server struct {
	d        Deps
	handlers map[handlerKey]http.Handler // (协议, Op) → core.Handler，构造后只读
}

type handlerKey struct {
	proto protocol.Protocol
	op    string
}

// New 创建服务端，并为每个工具所属入站适配器的每个调用端点预先创建内核处理器。
func New(d Deps) *Server {
	s := &Server{d: d, handlers: map[handlerKey]http.Handler{}}
	for _, t := range d.Registry.Tools() {
		for _, rt := range t.Ingress.Routes() {
			k := handlerKey{t.Ingress.Protocol(), rt.Op}
			if rt.Kind == protocol.RouteCall && s.handlers[k] == nil {
				s.handlers[k] = d.Core.Handler(t.Ingress, rt)
			}
		}
	}
	return s
}

// ServeHTTP 实现 SRV-M01 ~ SRV-M06：
//   - POST：读取一条 JSON-RPC 消息（上限 MaxMessageBytes）；通知与客户端响应 → 202 无响应体；
//     请求 → 200 application/json 的 JSON-RPC 响应（本服务端从不使用 SSE 回复）；
//   - GET → 405（不提供服务端主动推送流）；DELETE → 405（无会话可删除）；
//   - 请求头 MCP-Protocol-Version 存在且不受支持 → 400。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// SRV-M06: Origin 校验（防 DNS 重绑定）
	if origin := r.Header.Get("Origin"); origin != "" {
		if !isAllowedOrigin(origin) {
			writeJSON(w, http.StatusForbidden, ErrorResponse(nil, CodeInvalidRequest, "forbidden origin"))
			return
		}
	}

	// SRV-M05: GET 与 DELETE 返回 405
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// SRV-M03: MCP-Protocol-Version 头存在且不在 SupportedVersions 中返回 400
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" {
		if !slices.Contains(SupportedVersions, v) {
			writeJSON(w, http.StatusBadRequest, ErrorResponse(nil, CodeInvalidRequest, "unsupported protocol version"))
			return
		}
	}

	// SRV-M04: POST 读取不超过 MaxMessageBytes 的 JSON-RPC 请求
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxMessageBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse(nil, CodeParseError, "failed to read request body"))
		return
	}
	if int64(len(body)) > MaxMessageBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse(nil, CodeInvalidRequest, "request body too large"))
		return
	}

	req, err := DecodeRequest(body)
	if err != nil {
		if errors.Is(err, errNotRequest) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			writeJSON(w, http.StatusOK, ErrorResponse(nil, rpcErr.Code, rpcErr.Message))
			return
		}
		writeJSON(w, http.StatusOK, ErrorResponse(nil, CodeInvalidRequest, err.Error()))
		return
	}

	// 通知不回复（202 Accepted，无响应体）
	if req.IsNotification() || strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.dispatch(r, req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err == nil && u.Hostname() != "" {
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	host := origin
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// dispatch 按 method 分派（SRV-M07）：initialize、ping、tools/list、tools/call；
// notifications/* 不回复；其他 → CodeMethodNotFound。
func (s *Server) dispatch(r *http.Request, req *Request) *Response {
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		version := SupportedVersions[0]
		for _, sv := range SupportedVersions {
			if p.ProtocolVersion == sv {
				version = sv
				break
			}
		}
		res := map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools": map[string]any{
					"listChanged": false,
				},
			},
			"serverInfo": map[string]any{
				"name":    "universal-ai-gateway",
				"version": s.d.Version,
			},
			"instructions": "These tools are proxied by universal-ai-gateway. Tool calls are subject to metering and billing.",
		}
		return ResultResponse(req.ID, res)

	case "ping":
		return ResultResponse(req.ID, map[string]any{})

	case "tools/list":
		principal, _ := auth.PrincipalFrom(r.Context())
		tools := s.listTools(principal)
		return ResultResponse(req.ID, map[string]any{
			"tools": tools,
		})

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return ErrorResponse(req.ID, CodeInvalidParams, "invalid params")
			}
		}
		if p.Name == "" {
			return ErrorResponse(req.ID, CodeInvalidParams, "missing tool name")
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		res, rpcErr := s.callTool(r, p.Name, p.Arguments)
		if rpcErr != nil {
			return ErrorResponse(req.ID, rpcErr.Code, rpcErr.Message)
		}
		content := make([]map[string]any, 0, len(res.Content))
		for _, c := range res.Content {
			switch c.Type {
			case "image":
				content = append(content, map[string]any{
					"type":     "image",
					"data":     c.Data,
					"mimeType": c.MIMEType,
				})
			case "resource":
				content = append(content, map[string]any{
					"type": "resource",
					"resource": map[string]any{
						"uri":      c.URI,
						"mimeType": c.MIMEType,
						"text":     c.Text,
					},
				})
			default:
				content = append(content, map[string]any{
					"type": "text",
					"text": c.Text,
				})
			}
		}
		result := map[string]any{
			"content": content,
			"isError": res.IsError,
		}
		if len(res.Structured) > 0 && json.Valid(res.Structured) {
			result["structuredContent"] = res.Structured
		}
		return ResultResponse(req.ID, result)

	default:
		return ErrorResponse(req.ID, CodeMethodNotFound, "method not found")
	}
}

// WriteHTTPError 是 /mcp 端点中间件使用的错误格式（spec/14 SRV-M08）：
// HTTP 状态码取 err.Code.HTTPStatus()，响应体为 id 为 null 的 JSON-RPC 错误（code = CodeInternalError，message = err.Message），
// 401 时另加 `WWW-Authenticate: Bearer`。签名与 httpapi.ErrorWriter 相同。
func WriteHTTPError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	if err.Code == apperr.CodeUnauthenticated {
		w.Header().Set("WWW-Authenticate", `Bearer realm="universal-ai-gateway"`)
	}
	apperr.WriteJSON(w, err, ErrorResponse(nil, CodeInternalError, err.Message))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b) // 写客户端失败无法补救
}
