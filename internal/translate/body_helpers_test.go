package translate

import (
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// 以下辅助函数按「Decoder.Body → Encoder.Body」组合出各方向的非流式转换，与转换器的
// ResponseBuffered 路径（translateBuffered）走同一套代码，供按方向编写的旧测试使用。

func anthropicToChatBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	dec := anthropicDecoder{}
	enc := newChatEncoder(now, newID)
	resp, err := dec.Body(body)
	if err != nil {
		return nil, err
	}
	return enc.Body(resp, &protocol.Envelope{Model: model})
}

func anthropicToResponsesBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	dec := anthropicDecoder{}
	enc := newResponsesEncoder(now, newID)
	resp, err := dec.Body(body)
	if err != nil {
		return nil, err
	}
	return enc.Body(resp, &protocol.Envelope{Model: model})
}

func chatToResponsesBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	dec := chatDecoder{}
	enc := newResponsesEncoder(now, newID)
	resp, err := dec.Body(body)
	if err != nil {
		return nil, err
	}
	return enc.Body(resp, &protocol.Envelope{Model: model})
}

func chatToAnthropicBody(body []byte, model string, newID idGen) ([]byte, error) {
	dec := chatDecoder{}
	enc := newAnthropicEncoder(newID)
	resp, err := dec.Body(body)
	if err != nil {
		return nil, err
	}
	return enc.Body(resp, &protocol.Envelope{Model: model})
}
