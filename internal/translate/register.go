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
	for _, t := range []protocol.Translator{ChatToAnthropic{}, ResponsesToAnthropic{}, AnthropicToChat{}, ResponsesToChat{}} {
		if err := r.RegisterTranslator(t); err != nil {
			return fmt.Errorf("register translators: %w", err)
		}
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
	now, newID := clockOr(t.Now), idsOr(t.NewID)
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

// ChatToAnthropic 让 OpenAI Chat 客户端使用 Anthropic 上游（spec/10 §3 ~ §5，T20 + T21 + T22）。
// 无状态、并发安全；零值可用。
type ChatToAnthropic struct {
	// Now 与 NewID 仅供测试注入；为 nil 时使用 time.Now 与 crypto/rand 生成的 ID。
	Now   func() time.Time
	NewID func(prefix string) string
}

func (ChatToAnthropic) From() protocol.Protocol { return protoOpenAIChat }
func (ChatToAnthropic) To() protocol.Protocol   { return protoAnthropic }

// Request：Chat 请求 → IR（T20）→ Anthropic 请求（T21）。
func (t ChatToAnthropic) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	if env.Op != opChatCompletions {
		return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("operation %s is not supported for model %q", env.Op, env.Model))
	}
	req, err := lowerOpenAIChat(env.Body)
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

// Response：Anthropic 响应/流 → Chat 响应/流（T22，§5）。model 写客户端请求的服务名。
func (t ChatToAnthropic) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	now, newID := clockOr(t.Now), idsOr(t.NewID)
	switch resp.Kind {
	case protocol.ResponseBuffered:
		body, err := anthropicToChatBody(resp.Body, env.Model, now, newID)
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
			Events: newChatStream(resp.Events, env.Model, now, newID, includeUsage(env.Body)),
			Meter:  resp.Meter,
		}, nil
	default:
		return resp, nil // 字节流没有协议结构，原样返回
	}
}

// AnthropicToChat 让 Anthropic 客户端（Claude Code 等）使用只提供 OpenAI Chat 的上游（spec/10 §6，T23 + T24）。
// 无状态、并发安全；零值可用。
type AnthropicToChat struct {
	// NewID 仅供测试注入；为 nil 时使用 crypto/rand 生成的 ID。
	NewID func(prefix string) string
}

func (AnthropicToChat) From() protocol.Protocol { return protoAnthropic }
func (AnthropicToChat) To() protocol.Protocol   { return protoOpenAIChat }

// Request：Anthropic 请求 → IR（§6.1）→ Chat 请求（§6.2）。
func (t AnthropicToChat) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	if env.Op != opAnthropicCreate {
		return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("operation %s is not supported for model %q", env.Op, env.Model))
	}
	req, err := lowerAnthropic(env.Body)
	if err != nil {
		return nil, err
	}
	req.Model = upstreamModel
	body, err := synthesizeOpenAIChat(req)
	if err != nil {
		return nil, err
	}
	return &protocol.Call{Op: opChatCompletions, Model: upstreamModel, Stream: req.Stream, Body: body, Header: env.Header}, nil
}

// Response：Chat 响应/流 → Anthropic 响应/流（T24，§6.3）。model 写客户端请求的模型名。
func (t AnthropicToChat) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	newID := idsOr(t.NewID)
	switch resp.Kind {
	case protocol.ResponseBuffered:
		body, err := chatToAnthropicBody(resp.Body, env.Model, newID)
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
			Events: newAnthropicStream(resp.Events, env.Model, newID),
			Meter:  resp.Meter,
		}, nil
	default:
		return resp, nil // 字节流没有协议结构，原样返回
	}
}

// ResponsesToChat 让 Codex CLI 等只说 Responses 的客户端使用只提供 OpenAI Chat 的上游（spec/10 §8，T25 + T23 + T27）。
// 无状态、并发安全；零值可用。
type ResponsesToChat struct {
	// Now 与 NewID 仅供测试注入；为 nil 时使用 time.Now 与 crypto/rand 生成的 ID（RX-05）。
	Now   func() time.Time
	NewID func(prefix string) string
}

func (ResponsesToChat) From() protocol.Protocol { return protoOpenAIResponses }
func (ResponsesToChat) To() protocol.Protocol   { return protoOpenAIChat }

// Request：Responses 请求 → IR（T25）→ Chat 请求（§6.2，T23）。
func (t ResponsesToChat) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	if env.Op != opResponsesCreate {
		return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("operation %s is not supported for model %q", env.Op, env.Model))
	}
	req, err := lowerOpenAIResponses(env.Body)
	if err != nil {
		return nil, err
	}
	req.Model = upstreamModel
	body, err := synthesizeOpenAIChat(req)
	if err != nil {
		return nil, err
	}
	return &protocol.Call{Op: opChatCompletions, Model: upstreamModel, Stream: req.Stream, Body: body, Header: env.Header}, nil
}

// Response：Chat 响应/流 → Responses 响应/流（T27，§8.3）。model 写客户端请求的服务名。
func (t ResponsesToChat) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	now, newID := clockOr(t.Now), idsOr(t.NewID)
	switch resp.Kind {
	case protocol.ResponseBuffered:
		body, err := chatToResponsesBody(resp.Body, env.Model, now, newID)
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
			Events: newChatResponsesStream(resp.Events, env.Model, now, newID),
			Meter:  resp.Meter,
		}, nil
	default:
		return resp, nil // 字节流没有协议结构，原样返回
	}
}

func clockOr(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

func idsOr(newID func(string) string) idGen {
	if newID != nil {
		return newID
	}
	return newGatewayID
}
