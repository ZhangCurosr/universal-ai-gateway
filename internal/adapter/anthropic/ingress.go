package anthropic

import (
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是 Anthropic 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var _ protocol.Ingress = Ingress{}

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// OpMessages 是 Messages 调用的操作名。
const OpMessages = "messages"

// Routes 声明 POST /v1/messages 与 GET /v1/models（IN-A01）。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/messages", Kind: protocol.RouteCall, Op: OpMessages},
		{Method: http.MethodGet, Path: "/v1/models", Kind: protocol.RouteCatalog},
	}
}

// Claims 认领带 anthropic-version 头的请求（IN-A02，用于 GET /v1/models 与 OpenAI 冲突时）。
func (Ingress) Claims(r *http.Request) bool {
	return r.Header.Get("anthropic-version") != ""
}

// Parse 实现 IN-A03 ~ IN-A06。
func (Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	// TODO(T09A): 读取 model（必填字符串）、stream、max_tokens；Op = rt.Op；
	// Estimate = {input_tokens: pricing.EstimateInputTokens(len(body)), output_tokens: max_tokens（有则填）}；
	// 亲和线索按 spec/08 §2.3。
	return nil, apperr.NotImplemented("T09A")
}

var errorType = map[apperr.Code]string{
	apperr.CodeInvalidRequest:      "invalid_request_error",
	apperr.CodeUnauthenticated:     "authentication_error",
	apperr.CodeInsufficientQuota:   "billing_error",
	apperr.CodePermissionDenied:    "permission_error",
	apperr.CodeNotFound:            "not_found_error",
	apperr.CodeConflict:            "invalid_request_error",
	apperr.CodeRequestTooLarge:     "request_too_large",
	apperr.CodeRateLimited:         "rate_limit_error",
	apperr.CodeUpstreamUnavailable: "overloaded_error",
	apperr.CodeOverloaded:          "overloaded_error",
	apperr.CodeTimeout:             "timeout_error",
}

// ErrorType 返回错误码对应的 Anthropic error.type（未列出的一律为 api_error）。
func ErrorType(c apperr.Code) string {
	if t, ok := errorType[c]; ok {
		return t
	}
	return "api_error"
}

// WriteError 写 {"type":"error","error":{"type","message"},"request_id"}（spec/08 §2.4）。
func (Ingress) WriteError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{
		"type":       "error",
		"error":      map[string]any{"type": ErrorType(err.Code), "message": err.Message},
		"request_id": requestID,
	})
}

// StreamError 实现 spec/08 §2.5 的 Anthropic 错误事件。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	// TODO(T09A): event: error，data: {"type":"error","error":{"type":ErrorType(code),"message":...}}
	return sse.Event{}
}

// WriteCatalog 实现 spec/08 §2.6 的 Anthropic 格式模型列表。
func (Ingress) WriteCatalog(w http.ResponseWriter, models []protocol.ModelInfo) {
	// TODO(T09A)
	w.WriteHeader(http.StatusNotImplemented)
}
