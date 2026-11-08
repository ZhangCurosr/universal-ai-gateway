package protocol

import (
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是入站适配器：负责「客户端说的协议」的一切细节（spec/08 §2）。
// 实现必须无状态、并发安全。
type Ingress interface {
	// Protocol 返回协议名，必须与同名 Egress 一致（如果有）。
	Protocol() Protocol

	// Routes 声明本协议的 HTTP 端点：至少 1 个 RouteCall，至多 1 个 RouteCatalog（REG-03）。
	Routes() []Route

	// Claims 仅在多个 Ingress 声明了同一 (Method, Path) 时被调用，用于判定请求归属
	// （例如 GET /v1/models：带 anthropic-version 头的归 Anthropic）。返回 true 表示认领。
	Claims(r *http.Request) bool

	// Parse 把请求解析为 Envelope。rt 是命中的路由；body 是已读取并限长的请求体
	// （rt.Body 不是 BodyBuffered 时为 nil，此时只能从路径、查询串、请求头取信息）。
	// 请求不合法时返回 CodeInvalidRequest 的 *apperr.Error，消息说明哪个字段有问题。
	Parse(r *http.Request, rt Route, body []byte) (*Envelope, error)

	// WriteError 按本协议格式写错误响应（含状态码与 Retry-After）。只能在尚未写出响应头时调用。
	WriteError(w http.ResponseWriter, requestID string, err *apperr.Error)

	// StreamError 返回 SSE 流已开始后用于告知客户端出错的事件（spec/08 §2.5）。
	StreamError(err *apperr.Error) sse.Event

	// WriteCatalog 按本协议格式写目录（200）。models 已按 ID 升序排列，可能为空。
	// 没有声明 RouteCatalog 的协议实现为写 404 即可（不会被调用）。
	WriteCatalog(w http.ResponseWriter, models []ModelInfo)
}
