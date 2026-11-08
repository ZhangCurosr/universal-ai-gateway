package proxy

import (
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
)

// SessionHeader 是客户端显式指定会话的请求头（spec/07 §7 优先级 1）。
const SessionHeader = "X-UAIG-Session"

// AffinityKey 按 spec/07 §7 计算亲和键；所有来源都为空时返回 ""。
func AffinityKey(proto provider.Protocol, clientID int64, h http.Header, body []byte) string {
	// TODO(T10): 返回 "c<clientID>:<h|u|p>:<hex(sha256(value))[:16]>"。
	return ""
}
