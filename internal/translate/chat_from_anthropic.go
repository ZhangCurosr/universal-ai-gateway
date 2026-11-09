package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// 本文件实现 spec/10 §5：Anthropic 响应/流 → OpenAI Chat 响应/流（T22）。

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"` // 仅流式 delta 使用
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

// toChat 实现 §5.1 的 usage 映射：prompt = input + cache_read + cache_write。
func (dst anthropicUsage) toChat() *chatUsage {
	r := dst.toResponses()
	u := &chatUsage{PromptTokens: r.InputTokens, CompletionTokens: r.OutputTokens, TotalTokens: r.TotalTokens}
	u.PromptTokensDetails.CachedTokens = r.InputTokensDetails.CachedTokens
	return u
}

// finishReasonFor 实现 §5.1 的 stop_reason 映射。
//
// SPEC-GAP(T22): §5.1 未列出的 stop_reason（如 pause_turn）与缺失的 stop_reason 按 "stop" 处理。
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

// chatID 实现 §5.1：上游 id 的前缀改为 chatcmpl-；上游没有 id 时由网关签发。
func chatID(upstream string, newID idGen) string {
	if upstream == "" {
		return newID("chatcmpl-")
	}
	if rest, ok := strings.CutPrefix(upstream, "msg_"); ok {
		return "chatcmpl-" + rest
	}
	return "chatcmpl-" + upstream
}

// anthropicToChatBody 转换非流式 Anthropic 响应体（§5.1）。
func anthropicToChatBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
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

	type message struct {
		Role             string         `json:"role"`
		Content          *string        `json:"content"` // 没有文本时为 null
		ReasoningContent string         `json:"reasoning_content,omitempty"`
		ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	}
	msg := message{Role: "assistant"}
	var text, reasoning strings.Builder
	var hasText bool
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			hasText = true
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			tc := chatToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name, tc.Function.Arguments = b.Name, &args
			msg.ToolCalls = append(msg.ToolCalls, tc)
		default:
			// redacted_thinking 等：没有可展示的文本，丢弃
		}
	}
	if hasText {
		s := text.String()
		msg.Content = &s
	}
	msg.ReasoningContent = reasoning.String()

	out := map[string]any{
		"id": chatID(in.ID, newID), "object": "chat.completion", "created": now.Unix(), "model": model,
		"choices": []map[string]any{{
			"index": 0, "message": msg, "finish_reason": finishReasonFor(in.StopReason), "logprobs": nil,
		}},
		"usage": in.Usage.toChat(),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal chat body: %w", err)
	}
	return b, nil
}

// includeUsage 读取原始 Chat 请求中的 stream_options.include_usage（§5.2 message_stop）。
// 请求体在 Request 阶段已校验过，这里解析失败按未请求处理。
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

// ---- 流式（§5.2 状态机） ----

// chatStream 把 Anthropic 事件流转换为 OpenAI Chat chunk 流。非并发安全（EventStream 约定）。
type chatStream struct {
	in           protocol.EventStream
	model        string
	created      int64
	newID        idGen
	includeUsage bool

	id       string
	queue    []sse.Event
	usage    anthropicUsage
	started  bool
	finished bool // 已输出 finish_reason chunk
	done     bool

	tools map[int]int // Anthropic content block index → 本响应内工具调用序号
}

func newChatStream(in protocol.EventStream, model string, now time.Time, newID idGen, includeUsage bool) *chatStream {
	return &chatStream{in: in, model: model, created: now.Unix(), newID: newID, includeUsage: includeUsage, tools: map[int]int{}}
}

func (s *chatStream) Close() error { return s.in.Close() }

// Next 返回下一个 chunk。上游 error 事件或上游流中断返回 *protocol.AttemptError，
// 内核据此写出入站适配器的 StreamError（spec/08 §2.6）后结束，不发送 [DONE]。
func (s *chatStream) Next() (sse.Event, error) {
	for len(s.queue) == 0 {
		if s.done {
			return sse.Event{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 上游在 message_stop 之前结束：流被截断，不能伪造 [DONE]
				return sse.Event{}, &protocol.AttemptError{
					Outcome: protocol.Outcome{Kind: protocol.OutcomeNetworkError},
					Err:     errors.New("upstream stream ended before message_stop"),
				}
			}
			return sse.Event{}, err
		}
		if err := s.handle(ev); err != nil {
			return sse.Event{}, err
		}
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, nil
}

func (s *chatStream) handle(ev sse.Event) error {
	var data struct {
		Index   int `json:"index"`
		Message struct {
			ID    string         `json:"id"`
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage anthropicUsage `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError},
				Err: fmt.Errorf("decode anthropic event %q: %w", ev.Name, err)}
		}
	}

	switch ev.Name {
	case "message_start":
		s.usage.merge(data.Message.Usage)
		s.start(data.Message.ID)

	case "content_block_start":
		s.start("")
		switch data.ContentBlock.Type {
		case "text":
			if data.ContentBlock.Text != "" {
				s.delta(map[string]any{"content": data.ContentBlock.Text})
			}
		case "tool_use":
			i := len(s.tools)
			s.tools[data.Index] = i
			empty := ""
			tc := chatToolCall{Index: &i, ID: data.ContentBlock.ID, Type: "function"}
			tc.Function.Name, tc.Function.Arguments = data.ContentBlock.Name, &empty
			s.delta(map[string]any{"tool_calls": []chatToolCall{tc}})
		}

	case "content_block_delta":
		switch data.Delta.Type {
		case "text_delta":
			if data.Delta.Text != "" {
				s.delta(map[string]any{"content": data.Delta.Text})
			}
		case "thinking_delta":
			if data.Delta.Thinking != "" {
				s.delta(map[string]any{"reasoning_content": data.Delta.Thinking})
			}
		case "input_json_delta":
			i, ok := s.tools[data.Index]
			if ok && data.Delta.PartialJSON != "" {
				args := data.Delta.PartialJSON
				tc := chatToolCall{Index: &i}
				tc.Function.Arguments = &args
				s.delta(map[string]any{"tool_calls": []chatToolCall{tc}})
			}
		default:
			// signature_delta：§5.2 不输出
		}

	case "message_delta":
		s.usage.merge(data.Usage)
		if data.Delta.StopReason != "" {
			s.finish(data.Delta.StopReason)
		}

	case "message_stop":
		s.start("")
		s.finish("")
		if s.includeUsage {
			s.chunk([]map[string]any{}, s.usage.toChat())
		}
		s.queue = append(s.queue, sse.Event{Data: []byte("[DONE]")})
		s.done = true

	case "error":
		msg := data.Error.Message
		if msg == "" {
			msg = "upstream stream error"
		}
		return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: streamErrorKind(data.Error.Type)},
			Err: fmt.Errorf("anthropic stream error (%s): %s", data.Error.Type, msg)}

	default:
		// ping、content_block_stop 与未知事件：不输出
	}
	return nil
}

// start 输出首个 chunk（role=assistant），只输出一次；id 取 message_start 中的上游 id。
func (s *chatStream) start(upstreamID string) {
	if s.started {
		return
	}
	s.started = true
	s.id = chatID(upstreamID, s.newID)
	s.delta(map[string]any{"role": "assistant", "content": ""})
}

// finish 输出 finish_reason chunk（只输出一次；message_stop 前没有 message_delta 时按 "stop"）。
func (s *chatStream) finish(stopReason string) {
	if s.finished {
		return
	}
	s.finished = true
	s.chunk([]map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReasonFor(stopReason)}}, nil)
}

func (s *chatStream) delta(d map[string]any) {
	s.chunk([]map[string]any{{"index": 0, "delta": d, "finish_reason": nil}}, nil)
}

// chunk 追加一个 chat.completion.chunk：所有 chunk 的 id、created、model 相同（§5.2）。
func (s *chatStream) chunk(choices []map[string]any, usage *chatUsage) {
	c := map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model, "choices": choices}
	if usage != nil {
		c["usage"] = usage
	}
	b, err := json.Marshal(c)
	if err != nil { // 字段全部是本包定义的可序列化类型，不会失败
		b = []byte(`{"object":"chat.completion.chunk","choices":[]}`)
	}
	s.queue = append(s.queue, sse.Event{Data: b})
}
