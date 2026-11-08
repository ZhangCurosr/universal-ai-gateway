package mcp

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
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
	// TODO(T32)
	writeJSON(w, http.StatusOK, ErrorResponse(nil, CodeInternalError, "not implemented (T32)"))
}

// dispatch 按 method 分派（SRV-M07）：initialize、ping、tools/list、tools/call；
// notifications/* 不回复；其他 → CodeMethodNotFound。
func (s *Server) dispatch(r *http.Request, req *Request) *Response {
	// TODO(T32)
	return ErrorResponse(req.ID, CodeMethodNotFound, "method not found")
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
