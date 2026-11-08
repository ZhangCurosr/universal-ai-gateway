package apperr

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// Normalize 把任意错误转换为 *Error；非业务错误变为 internal（消息固定为 "internal error"，不泄露内部文本）。
func Normalize(err error) *Error {
	if e, ok := As(err); ok {
		return e
	}
	return Wrap(CodeInternal, "internal error", err)
}

// WriteJSON 写一个 JSON 错误响应：状态码取 e.Code.HTTPStatus()，e.RetryAfter > 0 时写 Retry-After（秒，向上取整）。
// body 的格式由调用方（各协议的入站适配器、管理面）决定。只能在尚未写出响应头时调用。
func WriteJSON(w http.ResponseWriter, e *Error, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(body) // 写失败说明客户端已断开，无需处理
}
