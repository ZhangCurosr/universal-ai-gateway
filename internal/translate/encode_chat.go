package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

type chatEncoder struct {
	now   time.Time
	newID idGen
}

func newChatEncoder(now time.Time, newID idGen) chatEncoder {
	return chatEncoder{now: now, newID: newID}
}

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string  `json:"name,omitempty"`
		Arguments *string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func chatID(upstream string, newID idGen) string {
	if upstream == "" {
		return newID("chatcmpl-")
	}
	if rest, ok := strings.CutPrefix(upstream, "msg_"); ok {
		return "chatcmpl-" + rest
	}
	return "chatcmpl-" + upstream
}

func finishReasonFor(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func includeUsage(body []byte) bool {
	var req struct {
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.StreamOptions != nil && req.StreamOptions.IncludeUsage
}

func anthropicToChatBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	dec := anthropicDecoder{}
	enc := newChatEncoder(now, newID)
	resp, err := dec.Body(body)
	if err != nil {
		return nil, err
	}
	return enc.Body(resp, &protocol.Envelope{Model: model})
}

func mapStopReasonToChat(r ir.StopReason) string {
	switch r {
	case ir.StopMaxTokens:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopRefusal:
		return "content_filter"
	default:
		return "stop"
	}
}

func tokenUsageToChat(u ir.TokenUsage) *chatUsage {
	prompt := u.Input + u.CacheRead + u.CacheWrite
	res := &chatUsage{
		PromptTokens:     prompt,
		CompletionTokens: u.Output,
		TotalTokens:      prompt + u.Output,
	}
	res.PromptTokensDetails.CachedTokens = u.CacheRead
	return res
}

func (e chatEncoder) Body(resp *ir.Response, env *protocol.Envelope) ([]byte, error) {
	now := e.now
	type message struct {
		Role             string         `json:"role"`
		Content          *string        `json:"content"`
		ReasoningContent string         `json:"reasoning_content,omitempty"`
		ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	}
	msg := message{Role: "assistant"}

	var text, reasoning strings.Builder
	var hasText bool

	for _, p := range resp.Parts {
		switch part := p.(type) {
		case ir.TextPart:
			hasText = true
			text.WriteString(part.Text)
		case ir.ThinkingPart:
			reasoning.WriteString(part.Text)
		case ir.ToolCallPart:
			args := "{}"
			if len(part.Arguments) > 0 {
				args = string(part.Arguments)
			}
			tc := chatToolCall{ID: part.ID, Type: "function"}
			tc.Function.Name, tc.Function.Arguments = part.Name, &args
			msg.ToolCalls = append(msg.ToolCalls, tc)
		}
	}
	if hasText {
		s := text.String()
		msg.Content = &s
	}
	msg.ReasoningContent = reasoning.String()

	out := map[string]any{
		"id":      chatID(resp.ID, e.newID),
		"object":  "chat.completion",
		"created": now.Unix(),
		"model":   env.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": mapStopReasonToChat(resp.StopReason),
			"logprobs":      nil,
		}},
		"usage": tokenUsageToChat(resp.Usage),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal chat body: %w", err)
	}
	return b, nil
}

func (e chatEncoder) Stream(in irStream, env *protocol.Envelope) protocol.EventStream {
	return newChatEncoderStream(in, env.Model, e.now, e.newID, includeUsage(env.Body))
}

type chatEncoderStream struct {
	in           irStream
	model        string
	created      int64
	newID        idGen
	includeUsage bool

	id       string
	queue    []sse.Event
	usage    ir.TokenUsage
	stop     string
	started  bool
	finished bool
	done     bool
}

func newChatEncoderStream(in irStream, model string, now time.Time, newID idGen, includeUsage bool) *chatEncoderStream {
	return &chatEncoderStream{
		in:           in,
		model:        model,
		created:      now.Unix(),
		newID:        newID,
		includeUsage: includeUsage,
	}
}

func (s *chatEncoderStream) Close() error {
	return s.in.Close()
}

func (s *chatEncoderStream) Next() (sse.Event, error) {
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

func (s *chatEncoderStream) start(upstreamID string) {
	if s.started {
		return
	}
	s.started = true
	s.id = chatID(upstreamID, s.newID)
	s.delta(map[string]any{"role": "assistant", "content": ""})
}

func (s *chatEncoderStream) delta(d map[string]any) {
	s.chunk([]map[string]any{{"index": 0, "delta": d, "finish_reason": nil}}, nil)
}

func (s *chatEncoderStream) finish(stopReason string) {
	if s.finished {
		return
	}
	s.finished = true
	if stopReason == "" {
		stopReason = "stop"
	}
	s.chunk([]map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": stopReason}}, nil)
}

func (s *chatEncoderStream) chunk(choices []map[string]any, usage *chatUsage) {
	c := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": choices,
	}
	if usage != nil {
		c["usage"] = usage
	}
	b, err := json.Marshal(c)
	if err != nil {
		b = []byte(`{"object":"chat.completion.chunk","choices":[]}`)
	}
	s.queue = append(s.queue, sse.Event{Data: b})
}

func (s *chatEncoderStream) handle(ev ir.StreamEvent) {
	switch ev.Kind {
	case ir.EventStart:
		s.start(ev.ID)
		if ev.Usage.Input != 0 || ev.Usage.Output != 0 || ev.Usage.CacheRead != 0 || ev.Usage.CacheWrite != 0 {
			s.usage = ev.Usage
		}

	case ir.EventText:
		s.start("")
		s.delta(map[string]any{"content": ev.Text})

	case ir.EventThinking:
		s.start("")
		s.delta(map[string]any{"reasoning_content": ev.Text})

	case ir.EventToolStart:
		s.start("")
		args := ""
		tc := chatToolCall{
			Index: &ev.ToolIndex,
			ID:    ev.ToolID,
			Type:  "function",
		}
		tc.Function.Name = ev.ToolName
		tc.Function.Arguments = &args
		s.delta(map[string]any{"tool_calls": []chatToolCall{tc}})

	case ir.EventToolArgs:
		s.start("")
		args := ev.Text
		tc := chatToolCall{Index: &ev.ToolIndex}
		tc.Function.Arguments = &args
		s.delta(map[string]any{"tool_calls": []chatToolCall{tc}})

	case ir.EventStop:
		s.stop = mapStopReasonToChat(ev.Stop)

	case ir.EventUsage:
		s.usage = ev.Usage

	case ir.EventEnd:
		s.start("")
		s.finish(s.stop)
		if s.includeUsage {
			s.chunk([]map[string]any{}, tokenUsageToChat(s.usage))
		}
		s.queue = append(s.queue, sse.Event{Data: []byte("[DONE]")})
		s.done = true
	}
}
