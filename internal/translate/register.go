package translate

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// Register 注册本包实现的全部跨协议转换器（spec/10 §0），由 cmd/gateway/adapters.go 调用。
func Register(r *protocol.Registry) error {
	if err := r.RegisterTranslator(ResponsesToAnthropic{}); err != nil {
		return fmt.Errorf("register translators: %w", err)
	}
	return nil
}

// ResponsesToAnthropic 让只说 OpenAI Responses 的客户端（Codex CLI 等）使用 Anthropic 上游（spec/10 §8，T25 + T21 + T26）。
// 无状态、并发安全；零值可用。
type ResponsesToAnthropic struct {
	// Now 与 NewID 仅供测试注入；为 nil 时使用 time.Now 与 crypto/rand 生成的 ID（RX-05）。
	Now   func() time.Time
	NewID func(prefix string) string
}

func (ResponsesToAnthropic) From() protocol.Protocol { return protoOpenAIResponses }
func (ResponsesToAnthropic) To() protocol.Protocol   { return protoAnthropic }

// Request：Responses 请求 → IR（T25）→ Anthropic 请求（T21）。
// RX-04：按 ID 的操作在转译路径上不会到达这里（内核 404），到达时按不支持处理。
func (t ResponsesToAnthropic) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	if env.Op != opResponsesCreate {
		return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("operation %s is not supported for model %q", env.Op, env.Model))
	}
	req, err := lowerOpenAIResponses(env.Body)
	if err != nil {
		return nil, err
	}
	req.Model = upstreamModel
	body, err := synthesizeAnthropic(req, nil)
	if err != nil {
		return nil, err
	}
	return &protocol.Call{Op: opAnthropicCreate, Model: upstreamModel, Stream: req.Stream, Body: body, Header: env.Header}, nil
}

// Response：Anthropic 响应/流 → Responses 响应/流（T26，§8.3）。model 写客户端请求的服务名。
func (t ResponsesToAnthropic) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	now, newID := t.clock(), t.ids()
	switch resp.Kind {
	case protocol.ResponseBuffered:
		body, err := anthropicToResponsesBody(resp.Body, env.Model, now, newID)
		if err != nil {
			return nil, err
		}
		return &protocol.Response{
			Kind: protocol.ResponseBuffered, Status: resp.Status,
			Header: http.Header{"Content-Type": {"application/json"}},
			Body:   body, Meter: resp.Meter,
		}, nil
	case protocol.ResponseEvents:
		return &protocol.Response{
			Kind:   protocol.ResponseEvents,
			Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Cache-Control": {"no-cache"}},
			Events: newResponsesStream(resp.Events, env.Model, now, newID),
			Meter:  resp.Meter,
		}, nil
	default:
		return resp, nil // 字节流没有协议结构，原样返回
	}
}

func (t ResponsesToAnthropic) clock() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t ResponsesToAnthropic) ids() idGen {
	if t.NewID != nil {
		return t.NewID
	}
	return newGatewayID
}
