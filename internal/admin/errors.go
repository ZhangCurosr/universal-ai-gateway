package admin

import (
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// writeError 写管理面错误响应 {"error":{"code","message"}}（spec/09 §4）。
// 非业务错误统一变为 internal，消息不泄露内部文本。
func writeError(w http.ResponseWriter, err error) {
	e := apperr.Normalize(err)
	apperr.WriteJSON(w, e, map[string]any{"error": map[string]any{"code": string(e.Code), "message": e.Message}})
}
