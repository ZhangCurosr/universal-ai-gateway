package openai

import (
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是 OpenAI Chat 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var _ protocol.Ingress = Ingress{}

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// OpChatCompletions 是 Chat Completions 调用的操作名。
const OpChatCompletions = "chat.completions"

// Routes 声明 POST /v1/chat/completions 与 GET /v1/models（IN-O01）。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/chat/completions", Kind: protocol.RouteCall, Op: OpChatCompletions},
		{Method: http.MethodGet, Path: "/v1/models", Kind: protocol.RouteCatalog},
	}
}

// Claims 永不主动认领：OpenAI 是兜底协议，冲突路由上没人认领时自动归它（IN-O02）。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 IN-O03 ~ IN-O06。
func (Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	// TODO(T09B): 读取 model（必填）、stream、max_completion_tokens 优先于 max_tokens；Op = rt.Op；
	// Estimate = {input_tokens: pricing.EstimateInputTokens(len(body)), output_tokens: 上述上限（有则填）}；
	// 亲和线索按 spec/08 §2.3。
	return nil, apperr.NotImplemented("T09B")
}

var errorType = map[apperr.Code]string{
	apperr.CodeInvalidRequest:    "invalid_request_error",
	apperr.CodeUnauthenticated:   "authentication_error",
	apperr.CodeInsufficientQuota: "insufficient_quota",
	apperr.CodePermissionDenied:  "permission_error",
	apperr.CodeNotFound:          "not_found_error",
	apperr.CodeConflict:          "invalid_request_error",
	apperr.CodeRequestTooLarge:   "invalid_request_error",
	apperr.CodeRateLimited:       "rate_limit_error",
}

// ErrorType 返回错误码对应的 OpenAI error.type（未列出的一律为 server_error）。
func ErrorType(c apperr.Code) string {
	if t, ok := errorType[c]; ok {
		return t
	}
	return "server_error"
}

// WriteError 写 {"error":{"message","type","param","code"}}（spec/08 §2.4）。
func (Ingress) WriteError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{"error": map[string]any{
		"message": err.Message, "type": ErrorType(err.Code), "param": nil, "code": string(err.Code),
	}})
}

// StreamError 实现 spec/08 §2.5 的 OpenAI 错误事件（之后不发送 [DONE]）。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	// TODO(T09B): data: {"error":{"message":...,"type":ErrorType(code),"code":string(code)}}
	return sse.Event{}
}

// WriteCatalog 实现 spec/08 §2.6 的 OpenAI 格式模型列表。
func (Ingress) WriteCatalog(w http.ResponseWriter, models []protocol.ModelInfo) {
	// TODO(T09B)
	w.WriteHeader(http.StatusNotImplemented)
}
