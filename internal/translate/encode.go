package translate

import (
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// encoder 将规范化的 IR 表示转换为客户端协议响应或 SSE 流（spec/10 §9）。
type encoder interface {
	Body(resp *ir.Response, env *protocol.Envelope) ([]byte, error)
	Stream(in irStream, env *protocol.Envelope) protocol.EventStream
}
