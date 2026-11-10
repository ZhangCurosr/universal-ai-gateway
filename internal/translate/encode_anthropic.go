package translate

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

type anthropicEncoder struct {
	newID idGen
}

func newAnthropicEncoder(newID idGen) anthropicEncoder {
	return anthropicEncoder{newID: newID}
}

func anthropicMsgID(upstream string, newID idGen) string {
	if upstream == "" {
		return newID("msg_")
	}
	if rest, ok := strings.CutPrefix(upstream, "chatcmpl-"); ok {
		return "msg_" + rest
	}
	return "msg_" + upstream
}

type anthropicOutUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

type anthropicOutTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicOutToolContent struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

type anthropicOutMessage struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Model        string             `json:"model"`
	Content      []any              `json:"content"`
	StopReason   *string            `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        *anthropicOutUsage `json:"usage,omitempty"`
}

type anthropicStartMessage struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []any             `json:"content"`
	StopReason   *string           `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        anthropicOutUsage `json:"usage"`
}

func tokenUsageToAnthropicOut(u ir.TokenUsage) *anthropicOutUsage {
	return &anthropicOutUsage{
		InputTokens:              u.Input,
		CacheCreationInputTokens: u.CacheWrite,
		CacheReadInputTokens:     u.CacheRead,
		OutputTokens:             u.Output,
	}
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

func (e anthropicEncoder) Body(resp *ir.Response, env *protocol.Envelope) ([]byte, error) {
	var stopReason *string
	hasRefusal := false
	for _, p := range resp.Parts {
		if tp, ok := p.(ir.TextPart); ok && tp.IsRefusal {
			hasRefusal = true
			break
		}
	}
	if hasRefusal {
		s := "refusal"
		stopReason = &s
	} else {
		switch resp.StopReason {
		case ir.StopMaxTokens:
			s := "max_tokens"
			stopReason = &s
		case ir.StopToolUse:
			s := "tool_use"
			stopReason = &s
		case ir.StopSequence:
			s := "stop_sequence"
			stopReason = &s
		case ir.StopRefusal:
			s := "refusal"
			stopReason = &s
		default:
			s := "end_turn"
			stopReason = &s
		}
	}

	content := make([]any, 0)
	for _, p := range resp.Parts {
		switch part := p.(type) {
		case ir.TextPart:
			content = append(content, anthropicOutTextContent{Type: "text", Text: part.Text})
		case ir.ToolCallPart:
			var input any = map[string]any{}
			if len(part.Arguments) > 0 {
				_ = json.Unmarshal(part.Arguments, &input)
			}
			content = append(content, anthropicOutToolContent{
				Type:  "tool_use",
				ID:    part.ID,
				Name:  part.Name,
				Input: input,
			})
		}
	}

	out := anthropicOutMessage{
		ID:           anthropicMsgID(resp.ID, e.newID),
		Type:         "message",
		Role:         "assistant",
		Model:        env.Model,
		Content:      content,
		StopReason:   stopReason,
		StopSequence: nil,
		Usage:        tokenUsageToAnthropicOut(resp.Usage),
	}
	return json.Marshal(out)
}

func (e anthropicEncoder) Stream(in irStream, env *protocol.Envelope) protocol.EventStream {
	return newAnthropicEncoderStream(in, env.Model, e.newID)
}

type anthropicEncoderStream struct {
	in      irStream
	model   string
	newID   idGen
	id      string
	queue   []sse.Event
	usage   ir.TokenUsage
	stop    ir.StopReason
	started bool
	done    bool

	nextIndex int
	hasText   bool
	textBlock int

	toolBlocks map[int]int
}

func newAnthropicEncoderStream(in irStream, model string, newID idGen) *anthropicEncoderStream {
	return &anthropicEncoderStream{
		in:         in,
		model:      model,
		newID:      newID,
		toolBlocks: make(map[int]int),
	}
}

func (s *anthropicEncoderStream) Close() error {
	return s.in.Close()
}

func (s *anthropicEncoderStream) Next() (sse.Event, error) {
	for len(s.queue) == 0 {
		if s.done {
			return sse.Event{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			return sse.Event{}, err
		}
		s.handle(ev)
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, nil
}

func (s *anthropicEncoderStream) emit(name string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.queue = append(s.queue, sse.Event{
		Name: name,
		Data: b,
	})
}

func (s *anthropicEncoderStream) handle(ev ir.StreamEvent) {
	switch ev.Kind {
	case ir.EventStart:
		s.id = anthropicMsgID(ev.ID, s.newID)
		s.emit("message_start", map[string]any{
			"type": "message_start",
			"message": anthropicStartMessage{
				ID:           s.id,
				Type:         "message",
				Role:         "assistant",
				Model:        s.model,
				Content:      []any{},
				StopReason:   nil,
				StopSequence: nil,
				Usage:        anthropicOutUsage{},
			},
		})
		s.started = true

	case ir.EventText:
		if !s.hasText {
			s.hasText = true
			s.textBlock = s.nextIndex
			s.nextIndex++
			s.emit("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": s.textBlock,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
		}
		s.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.textBlock,
			"delta": map[string]any{
				"type": "text_delta",
				"text": ev.Text,
			},
		})

	case ir.EventThinking:
		// Anthropic 客户端不接收来自 Chat 的思考增量（spec/10 §6.5）

	case ir.EventToolStart:
		if s.hasText {
			s.emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": s.textBlock,
			})
			s.hasText = false
		}
		blockIdx := s.nextIndex
		s.nextIndex++
		s.toolBlocks[ev.ToolIndex] = blockIdx
		s.emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIdx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    ev.ToolID,
				"name":  ev.ToolName,
				"input": map[string]any{},
			},
		})

	case ir.EventToolArgs:
		blockIdx := s.toolBlocks[ev.ToolIndex]
		s.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIdx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": ev.Text,
			},
		})

	case ir.EventToolEnd:
		if blockIdx, ok := s.toolBlocks[ev.ToolIndex]; ok {
			s.emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIdx,
			})
		}

	case ir.EventStop:
		s.stop = ev.Stop

	case ir.EventUsage:
		s.usage = ev.Usage

	case ir.EventEnd:
		if s.hasText {
			s.emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": s.textBlock,
			})
			s.hasText = false
		}
		for _, b := range s.toolBlocks {
			_ = b
		}
		stopReason := "end_turn"
		switch s.stop {
		case ir.StopMaxTokens:
			stopReason = "max_tokens"
		case ir.StopToolUse:
			stopReason = "tool_use"
		case ir.StopSequence:
			stopReason = "stop_sequence"
		case ir.StopRefusal:
			stopReason = "refusal"
		default:
			stopReason = "end_turn"
		}

		s.emit("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": tokenUsageToAnthropicOut(s.usage),
		})
		s.emit("message_stop", map[string]any{"type": "message_stop"})
		s.done = true
	}
}
