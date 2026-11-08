package apperr

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// Format 是错误响应体的格式（spec/09 §2）。
type Format int

const (
	FormatOpenAI    Format = iota // {"error":{"message","type","param","code"}}
	FormatAnthropic               // {"type":"error","error":{"type","message"},"request_id"}
	FormatAdmin                   // {"error":{"code","message"}}
)

var anthropicType = map[Code]string{
	CodeInvalidRequest:      "invalid_request_error",
	CodeUnauthenticated:     "authentication_error",
	CodeInsufficientQuota:   "billing_error",
	CodePermissionDenied:    "permission_error",
	CodeNotFound:            "not_found_error",
	CodeConflict:            "invalid_request_error",
	CodeRequestTooLarge:     "request_too_large",
	CodeRateLimited:         "rate_limit_error",
	CodeInternal:            "api_error",
	CodeNotImplemented:      "api_error",
	CodeUpstreamError:       "api_error",
	CodeUpstreamUnavailable: "overloaded_error",
	CodeOverloaded:          "overloaded_error",
	CodeTimeout:             "timeout_error",
}

var openAIType = map[Code]string{
	CodeInvalidRequest:    "invalid_request_error",
	CodeUnauthenticated:   "authentication_error",
	CodeInsufficientQuota: "insufficient_quota",
	CodePermissionDenied:  "permission_error",
	CodeNotFound:          "not_found_error",
	CodeConflict:          "invalid_request_error",
	CodeRequestTooLarge:   "invalid_request_error",
	CodeRateLimited:       "rate_limit_error",
}

// AnthropicType 返回错误码对应的 Anthropic error.type。
func AnthropicType(c Code) string {
	if t, ok := anthropicType[c]; ok {
		return t
	}
	return "api_error"
}

// OpenAIType 返回错误码对应的 OpenAI error.type。
func OpenAIType(c Code) string {
	if t, ok := openAIType[c]; ok {
		return t
	}
	return "server_error"
}

// Normalize 把任意错误转换为 *Error；非业务错误变为 internal（消息固定为 "internal error"，不泄露内部文本）。
func Normalize(err error) *Error {
	if e, ok := As(err); ok {
		return e
	}
	return Wrap(CodeInternal, "internal error", err)
}

// WriteHTTP 按 format 写错误响应（spec/09 §2.4）。只能在尚未写出响应头时调用。
func WriteHTTP(w http.ResponseWriter, format Format, requestID string, err error) {
	e := Normalize(err)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(Body(format, requestID, e)) // 写失败说明客户端已断开，无需处理
}

// Body 返回错误响应体（便于测试与流式场景复用）。
func Body(format Format, requestID string, e *Error) any {
	switch format {
	case FormatAnthropic:
		return map[string]any{
			"type":       "error",
			"error":      map[string]any{"type": AnthropicType(e.Code), "message": e.Message},
			"request_id": requestID,
		}
	case FormatAdmin:
		return map[string]any{"error": map[string]any{"code": string(e.Code), "message": e.Message}}
	default:
		return map[string]any{"error": map[string]any{
			"message": e.Message, "type": OpenAIType(e.Code), "param": nil, "code": string(e.Code),
		}}
	}
}
