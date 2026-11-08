package openairesp

import (
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是 OpenAI Responses 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var (
	_ protocol.Ingress    = Ingress{}
	_ protocol.ToolSource = Ingress{}
)

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// Routes 实现 IN-R01。不声明目录路由：GET /v1/models 由 openai_chat 提供（同一格式）。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/responses", Kind: protocol.RouteCall, Op: OpCreate},
		{Method: http.MethodGet, Path: "/v1/responses/{id}", Kind: protocol.RouteCall, Op: OpGet, Body: protocol.BodyNone},
		{Method: http.MethodDelete, Path: "/v1/responses/{id}", Kind: protocol.RouteCall, Op: OpDelete, Body: protocol.BodyNone},
		{Method: http.MethodPost, Path: "/v1/responses/{id}/cancel", Kind: protocol.RouteCall, Op: OpCancel, Body: protocol.BodyNone},
		{Method: http.MethodGet, Path: "/v1/responses/{id}/input_items", Kind: protocol.RouteCall, Op: OpInputItems, Body: protocol.BodyNone},
	}
}

// Claims 不认领：本协议的路由不与其他协议冲突。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 IN-R02 ~ IN-R06。
func (Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	// TODO(T09C):
	//   responses.create：IN-L01 ~ IN-L03；Estimate = {input_tokens: EstimateInputTokens(len(body)), output_tokens: max_output_tokens（有则填）}；
	//     previous_response_id 非空 → Affinity = {AffinityPin, 该 ID, Strict}（IN-R03）；否则亲和线索按 IN-R04。
	//   其他操作：Model = r.Header.Get("X-UAIG-Model")（可以为空，IN-R05）；Params = {"id": r.PathValue("id"), "query": r.URL.RawQuery}；
	//     Estimate = 全部单位显式为 0；Affinity = {AffinityPin, id, Strict}。
	return nil, apperr.NotImplemented("T09C")
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

// ErrorType 返回错误码对应的 error.type（未列出的一律为 server_error），与 openai_chat 一致。
func ErrorType(c apperr.Code) string {
	if t, ok := errorType[c]; ok {
		return t
	}
	return "server_error"
}

// WriteError 写 {"error":{"message","type","param","code"}}（与 Chat Completions 相同的格式）。
func (Ingress) WriteError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{"error": map[string]any{
		"message": err.Message, "type": ErrorType(err.Code), "param": nil, "code": string(err.Code),
	}})
}

// StreamError 实现 IN-R07：event: error，data: {"type":"error","code":<错误码>,"message":<消息>,"param":null}。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	// TODO(T09C)
	return sse.Event{}
}

// WriteCatalog 不会被调用（没有目录路由）。
func (Ingress) WriteCatalog(w http.ResponseWriter, _ []protocol.ModelInfo) {
	w.WriteHeader(http.StatusNotFound)
}
