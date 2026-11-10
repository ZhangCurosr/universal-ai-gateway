package translate

import (
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// decoder 将上游响应体或 SSE 流解析为规范化的 IR 表示（spec/10 §9）。
type decoder interface {
	Body(body []byte) (*ir.Response, error)
	Stream(in protocol.EventStream) irStream
}

// irStream 是规范化的流式 IR 事件迭代器。
type irStream interface {
	Next() (ir.StreamEvent, error)
	Close() error
}

func streamFail(kind protocol.OutcomeKind, err error) error {
	return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: kind}, Err: err}
}
