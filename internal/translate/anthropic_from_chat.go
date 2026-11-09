package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// 本文件实现 spec/10 §6.3：OpenAI Chat 响应/流 → Anthropic 响应/流（T24）。

// chatInUsage 是 Chat usage 对象中本转换器读取的字段。
type chatInUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// DeepSeek 的缓存命中字段；与 openai 出站适配器的 USE-03 计量口径一致，仅在缺少 cached_tokens 时使用
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
}

func (u chatInUsage) cached() int64 {
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		return *u.PromptTokensDetails.CachedTokens
	}
	return u.PromptCacheHitTokens
}

// anthropicOutUsage 是 Anthropic usage 对象（§6.3 usage 映射）。
type anthropicOutUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

func (u chatInUsage) toAnthropic() anthropicOutUsage {
	c := u.cached()
	return anthropicOutUsage{InputTokens: max(u.PromptTokens-c, 0), CacheReadInputTokens: c, OutputTokens: u.CompletionTokens}
}

// stopReasonFor 实现 §6.3 的 finish_reason 映射。
//
// spec/10 §6.3：未列出的非标准中断原因（如 insufficient_system_resource）及缺失值视为上游故障（非流式 502，流式流错误）。
func stopReasonFor(finish string) (string, error) {
	switch finish {
	case "stop":
		return "end_turn", nil
	case "length":
		return "max_tokens", nil
	case "tool_calls", "function_call":
		return "tool_use", nil
	case "content_filter":
		return "refusal", nil
	default:
		return "", upstreamInvalid(fmt.Sprintf("upstream returned unrecognised finish_reason %q", finish))
	}
}

// anthropicMsgID 实现 §6.3：chatcmpl- 前缀改为 msg_；缺失时网关签发。
func anthropicMsgID(upstream string, newID idGen) string {
	if upstream == "" {
		return newID("msg_")
	}
	if rest, ok := strings.CutPrefix(upstream, "chatcmpl-"); ok {
		return "msg_" + rest
	}
	return "msg_" + upstream
}

// toolInput 把 Chat 的 arguments 字符串转换为 Anthropic tool_use.input（JSON 对象）；空串视为 {}。
func toolInput(args string) (json.RawMessage, bool) {
	t := bytes.TrimSpace([]byte(args))
	if len(t) == 0 {
		return json.RawMessage(`{}`), true
	}
	if !json.Valid(t) || t[0] != '{' {
		return nil, false
	}
	return json.RawMessage(t), true
}

func upstreamInvalid(msg string) error { return apperr.New(apperr.CodeUpstreamError, msg) }

type anthropicOutBlock struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicOutMessage struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	Role         string              `json:"role"`
	Model        string              `json:"model"`
	Content      []anthropicOutBlock `json:"content"`
	StopReason   *string             `json:"stop_reason"`
	StopSequence *string             `json:"stop_sequence"`
	Usage        anthropicOutUsage   `json:"usage"`
}

func textOut(s string) anthropicOutBlock { return anthropicOutBlock{Type: "text", Text: &s} }

// chatToAnthropicBody 转换非流式 Chat 响应体（§6.3 非流式）。
func chatToAnthropicBody(body []byte, model string, newID idGen) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				Refusal   *string `json:"refusal"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				// reasoning_content：§6.3 丢弃
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage chatInUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	if len(in.Choices) != 1 {
		return nil, upstreamInvalid(fmt.Sprintf("upstream returned %d choices, want exactly 1", len(in.Choices)))
	}
	c := in.Choices[0]
	stop, err := stopReasonFor(c.FinishReason)
	if err != nil {
		return nil, err
	}
	out := anthropicOutMessage{ID: anthropicMsgID(in.ID, newID), Type: "message", Role: "assistant", Model: model,
		Content: []anthropicOutBlock{}, StopReason: &stop, Usage: in.Usage.toAnthropic()}
	if m := c.Message.Content; m != nil && *m != "" {
		out.Content = append(out.Content, textOut(*m))
	}
	if r := c.Message.Refusal; r != nil && *r != "" {
		out.Content = append(out.Content, textOut(*r))
		stop = "refusal"
	}
	for i, tc := range c.Message.ToolCalls {
		input, ok := toolInput(tc.Function.Arguments)
		if !ok {
			return nil, upstreamInvalid(fmt.Sprintf("upstream tool_calls[%d].function.arguments is not a JSON object", i))
		}
		out.Content = append(out.Content, anthropicOutBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic body: %w", err)
	}
	return b, nil
}

// ---- 流式（§6.3 状态机） ----

// anthropicStream 把 Chat chunk 流转换为 Anthropic 事件流。非并发安全（EventStream 约定）。
type anthropicStream struct {
	in    protocol.EventStream
	model string
	newID idGen

	queue   []sse.Event
	usage   chatInUsage
	stop    string // 已收到的 finish_reason；空 = 尚未收到
	started bool
	done    bool

	blockIdx  int    // 下一个 content block 的 index
	open      string // 当前打开的块类型："" | "text" | "tool_use"
	openTool  int    // open == "tool_use" 时对应的 Chat tool_calls index
	seenTools map[int]bool
}

func newAnthropicStream(in protocol.EventStream, model string, newID idGen) *anthropicStream {
	return &anthropicStream{in: in, model: model, newID: newID, seenTools: map[int]bool{}}
}

func (s *anthropicStream) Close() error { return s.in.Close() }

func streamFail(kind protocol.OutcomeKind, err error) error {
	return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: kind}, Err: err}
}

// Next 返回下一个 Anthropic 事件。上游错误 chunk、流截断或在 finish_reason 之前收到 [DONE] 返回
// *protocol.AttemptError，内核据此写出入站适配器的 StreamError（event: error）后结束。
func (s *anthropicStream) Next() (sse.Event, error) {
	for len(s.queue) == 0 {
		if s.done {
			return sse.Event{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sse.Event{}, streamFail(protocol.OutcomeNetworkError, errors.New("upstream stream ended before [DONE]"))
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

func (s *anthropicStream) handle(ev sse.Event) error {
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		if s.stop == "" {
			return streamFail(protocol.OutcomeServerError, errors.New("upstream sent [DONE] without finish_reason"))
		}
		s.start("")
		stop, err := stopReasonFor(s.stop)
		if err != nil {
			return streamFail(protocol.OutcomeServerError, err)
		}
		s.emit("message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
			"usage": s.usage.toAnthropic(),
		})
		s.emit("message_stop", map[string]any{})
		s.done = true
		return nil
	}

	var chunk struct {
		ID      string `json:"id"`
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				// reasoning_content：§6.3 丢弃
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *chatInUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return streamFail(protocol.OutcomeServerError, fmt.Errorf("decode chat chunk: %w", err))
	}
	if chunk.Error != nil {
		return streamFail(protocol.OutcomeServerError, fmt.Errorf("chat stream error (%s): %s", chunk.Error.Type, chunk.Error.Message))
	}
	s.start(chunk.ID)
	if chunk.Usage != nil {
		s.usage = *chunk.Usage
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			if s.open != "text" {
				s.closeBlock()
				s.openBlock("text", map[string]any{"type": "text", "text": ""})
			}
			s.emit("content_block_delta", map[string]any{"index": s.blockIdx - 1,
				"delta": map[string]any{"type": "text_delta", "text": c.Delta.Content}})
		}
		for _, tc := range c.Delta.ToolCalls {
			if !s.seenTools[tc.Index] {
				s.seenTools[tc.Index] = true
				s.closeBlock()
				s.openTool = tc.Index
				s.openBlock("tool_use", map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}})
			} else if s.open != "tool_use" || s.openTool != tc.Index {
				return streamFail(protocol.OutcomeServerError, fmt.Errorf("upstream interleaved arguments for tool call %d", tc.Index))
			}
			if tc.Function.Arguments != "" {
				s.emit("content_block_delta", map[string]any{"index": s.blockIdx - 1,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			s.closeBlock()
			if _, err := stopReasonFor(*c.FinishReason); err != nil {
				return streamFail(protocol.OutcomeServerError, err)
			}
			s.stop = *c.FinishReason
		}
	}
	return nil
}

// start 输出 message_start（只输出一次）；真实用量在 message_delta 中给出（§6.3）。
func (s *anthropicStream) start(upstreamID string) {
	if s.started {
		return
	}
	s.started = true
	s.emit("message_start", map[string]any{"message": anthropicOutMessage{
		ID: anthropicMsgID(upstreamID, s.newID), Type: "message", Role: "assistant", Model: s.model,
		Content: []anthropicOutBlock{},
	}})
}

func (s *anthropicStream) openBlock(typ string, block map[string]any) {
	s.open = typ
	s.emit("content_block_start", map[string]any{"index": s.blockIdx, "content_block": block})
	s.blockIdx++
}

func (s *anthropicStream) closeBlock() {
	if s.open == "" {
		return
	}
	s.open = ""
	s.emit("content_block_stop", map[string]any{"index": s.blockIdx - 1})
}

// emit 追加一个事件：event 名与 data.type 相同（Anthropic 流格式）。
func (s *anthropicStream) emit(typ string, fields map[string]any) {
	fields["type"] = typ
	b, err := json.Marshal(fields)
	if err != nil { // 字段全部是本包定义的可序列化类型，不会失败
		b = []byte(`{"type":"` + typ + `"}`)
	}
	s.queue = append(s.queue, sse.Event{Name: typ, Data: b})
}
