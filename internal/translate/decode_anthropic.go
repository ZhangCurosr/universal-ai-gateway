package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

type anthropicDecoder struct{}

// anthropicUsage 是 Anthropic usage 对象中计入用量的字段。
type anthropicUsage struct {
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_input_tokens"`
	CacheWriteTokens *int64 `json:"cache_creation_input_tokens"`
}

// merge 用 u 中出现的字段覆盖 dst（message_delta 中的 usage 是累计值）。
func (dst *anthropicUsage) merge(u anthropicUsage) {
	for _, p := range []struct{ src, dst **int64 }{
		{&u.InputTokens, &dst.InputTokens}, {&u.OutputTokens, &dst.OutputTokens},
		{&u.CacheReadTokens, &dst.CacheReadTokens}, {&u.CacheWriteTokens, &dst.CacheWriteTokens},
	} {
		if *p.src != nil {
			*p.dst = *p.src
		}
	}
}

func streamErrorKind(t string) protocol.OutcomeKind {
	switch t {
	case "overloaded_error":
		return protocol.OutcomeOverloaded
	case "rate_limit_error":
		return protocol.OutcomeRateLimited
	default:
		return protocol.OutcomeServerError
	}
}

func mapAnthropicStopReason(r string) ir.StopReason {
	switch r {
	case "max_tokens":
		return ir.StopMaxTokens
	case "tool_use":
		return ir.StopToolUse
	case "stop_sequence":
		return ir.StopSequence
	case "refusal":
		return ir.StopRefusal
	default:
		return ir.StopEndTurn
	}
}

func stripAnthropicID(upstream string) string {
	if rest, ok := strings.CutPrefix(upstream, "msg_"); ok {
		return rest
	}
	return upstream
}

func (dst anthropicUsage) toIR() ir.TokenUsage {
	v := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return ir.TokenUsage{
		Input:      v(dst.InputTokens),
		CacheRead:  v(dst.CacheReadTokens),
		CacheWrite: v(dst.CacheWriteTokens),
		Output:     v(dst.OutputTokens),
	}
}

func (d anthropicDecoder) Body(body []byte) (*ir.Response, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}

	res := &ir.Response{
		ID:         stripAnthropicID(in.ID),
		Model:      in.Model,
		StopReason: mapAnthropicStopReason(in.StopReason),
		Usage:      in.Usage.toIR(),
	}
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				res.Parts = append(res.Parts, ir.TextPart{Text: b.Text})
			}
		case "thinking":
			if b.Thinking != "" {
				res.Parts = append(res.Parts, ir.ThinkingPart{
					Vendor: ir.VendorAnthropic,
					Text:   b.Thinking,
				})
			}
		case "tool_use":
			args := json.RawMessage(`{}`)
			if len(b.Input) > 0 {
				args = b.Input
			}
			res.Parts = append(res.Parts, ir.ToolCallPart{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: args,
			})
		}
	}
	return res, nil
}

func (d anthropicDecoder) Stream(in protocol.EventStream) irStream {
	return newAnthropicIRStream(in)
}

type anthropicIRStream struct {
	in    protocol.EventStream
	queue []ir.StreamEvent
	usage anthropicUsage
	done  bool

	tools map[int]int // content block index -> tool index
}

func newAnthropicIRStream(in protocol.EventStream) *anthropicIRStream {
	return &anthropicIRStream{
		in:    in,
		tools: make(map[int]int),
	}
}

func (s *anthropicIRStream) Close() error {
	return s.in.Close()
}

func (s *anthropicIRStream) Next() (ir.StreamEvent, error) {
	for len(s.queue) == 0 {
		if s.done {
			return ir.StreamEvent{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ir.StreamEvent{}, &protocol.AttemptError{
					Outcome: protocol.Outcome{Kind: protocol.OutcomeNetworkError},
					Err:     errors.New("upstream stream ended before message_stop"),
				}
			}
			return ir.StreamEvent{}, err
		}
		if err := s.handle(ev); err != nil {
			return ir.StreamEvent{}, err
		}
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, nil
}

func (s *anthropicIRStream) emit(ev ir.StreamEvent) {
	s.queue = append(s.queue, ev)
}

func (s *anthropicIRStream) handle(ev sse.Event) error {
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil
	}

	name := ev.Name
	if name == "" {
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &peek); err == nil && peek.Type != "" {
			name = peek.Type
		}
	}

	switch name {
	case "message_start":
		var chunk struct {
			Message struct {
				ID    string         `json:"id"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError}, Err: err}
		}
		s.usage.merge(chunk.Message.Usage)
		s.emit(ir.StreamEvent{
			Kind:  ir.EventStart,
			ID:    stripAnthropicID(chunk.Message.ID),
			Usage: s.usage.toIR(),
		})

	case "content_block_start":
		var chunk struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
				ID       string `json:"id"`
				Name     string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError}, Err: err}
		}
		switch chunk.ContentBlock.Type {
		case "text":
			if chunk.ContentBlock.Text != "" {
				s.emit(ir.StreamEvent{Kind: ir.EventText, Text: chunk.ContentBlock.Text})
			}
		case "thinking":
			if chunk.ContentBlock.Thinking != "" {
				s.emit(ir.StreamEvent{Kind: ir.EventThinking, Text: chunk.ContentBlock.Thinking})
			}
		case "tool_use":
			toolIdx := len(s.tools)
			s.tools[chunk.Index] = toolIdx
			s.emit(ir.StreamEvent{
				Kind:      ir.EventToolStart,
				ToolIndex: toolIdx,
				ToolID:    chunk.ContentBlock.ID,
				ToolName:  chunk.ContentBlock.Name,
			})
		}

	case "content_block_delta":
		var chunk struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError}, Err: err}
		}
		switch chunk.Delta.Type {
		case "text_delta":
			if chunk.Delta.Text != "" {
				s.emit(ir.StreamEvent{Kind: ir.EventText, Text: chunk.Delta.Text})
			}
		case "thinking_delta":
			if chunk.Delta.Thinking != "" {
				s.emit(ir.StreamEvent{Kind: ir.EventThinking, Text: chunk.Delta.Thinking})
			}
		case "input_json_delta":
			if chunk.Delta.PartialJSON != "" {
				toolIdx, ok := s.tools[chunk.Index]
				if !ok {
					toolIdx = len(s.tools)
					s.tools[chunk.Index] = toolIdx
				}
				s.emit(ir.StreamEvent{
					Kind:      ir.EventToolArgs,
					ToolIndex: toolIdx,
					Text:      chunk.Delta.PartialJSON,
				})
			}
		}

	case "content_block_stop":
		var chunk struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(data, &chunk); err == nil {
			if toolIdx, ok := s.tools[chunk.Index]; ok {
				s.emit(ir.StreamEvent{
					Kind:      ir.EventToolEnd,
					ToolIndex: toolIdx,
				})
			}
		}

	case "message_delta":
		var chunk struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError}, Err: err}
		}
		s.usage.merge(chunk.Usage)
		if chunk.Delta.StopReason != "" {
			s.emit(ir.StreamEvent{
				Kind: ir.EventStop,
				Stop: mapAnthropicStopReason(chunk.Delta.StopReason),
			})
		}
		s.emit(ir.StreamEvent{
			Kind:  ir.EventUsage,
			Usage: s.usage.toIR(),
		})

	case "message_stop":
		s.emit(ir.StreamEvent{Kind: ir.EventEnd})
		s.done = true

	case "error":
		var errEv struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &errEv); err != nil {
			return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError}, Err: err}
		}
		msg := errEv.Error.Message
		if msg == "" {
			msg = "upstream stream error"
		}
		return &protocol.AttemptError{
			Outcome: protocol.Outcome{Kind: streamErrorKind(errEv.Error.Type)},
			Err:     fmt.Errorf("anthropic stream error (%s): %s", errEv.Error.Type, msg),
		}
	}
	return nil
}
