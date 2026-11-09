package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// 本文件实现 spec/10 §8.3：Anthropic 响应/流 → OpenAI Responses 响应/流（T26）。

// idGen 生成网关签发的 ID（RX-05）；测试注入确定性实现。
type idGen func(prefix string) string

// respOutputItem 是 Responses 的一个输出项（message 或 function_call）。
type respOutputItem struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Role      string            `json:"role,omitempty"`
	Content   []respContentPart `json:"content,omitempty"`
	CallID    string            `json:"call_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Arguments *string           `json:"arguments,omitempty"`
}

type respContentPart struct {
	Type        string `json:"type"` // output_text
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type respUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type respIncomplete struct {
	Reason string `json:"reason"`
}

// respObject 是 Responses 的 response 对象（非流式响应体，以及流式事件中的 response 字段）。
type respObject struct {
	ID                string           `json:"id"`
	Object            string           `json:"object"`
	CreatedAt         int64            `json:"created_at"`
	Status            string           `json:"status"`
	Model             string           `json:"model"`
	Output            []respOutputItem `json:"output"`
	Usage             *respUsage       `json:"usage,omitempty"`
	IncompleteDetails *respIncomplete  `json:"incomplete_details"`
	Store             bool             `json:"store"` // RX-02：恒为 false
}

// anthropicUsage 是 Anthropic usage 对象中计入 Responses usage 的字段。
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

// toResponses 实现 §8.3 的 usage 映射。
func (dst anthropicUsage) toResponses() *respUsage {
	v := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	u := &respUsage{
		InputTokens:  v(dst.InputTokens) + v(dst.CacheReadTokens) + v(dst.CacheWriteTokens),
		OutputTokens: v(dst.OutputTokens),
	}
	u.InputTokensDetails.CachedTokens = v(dst.CacheReadTokens)
	u.TotalTokens = u.InputTokens + u.OutputTokens
	return u
}

// statusFor 实现 §8.3 的停止原因映射：达到输出上限 → incomplete，其余 → completed。
//
// SPEC-GAP(T26): §8.3 只规定了正常结束、工具调用与输出上限三种情况；refusal、pause_turn 等
// 其他 stop_reason 按 completed 处理（不猜测 incomplete_details.reason）。
func statusFor(stopReason string) (string, *respIncomplete) {
	if stopReason == "max_tokens" {
		return "incomplete", &respIncomplete{Reason: "max_output_tokens"}
	}
	return "completed", nil
}

// anthropicToResponsesBody 转换非流式 Anthropic 响应体（§8.3 非流式）。
func anthropicToResponsesBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	var in struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}

	out := respObject{ID: newID("resp_"), Object: "response", CreatedAt: now.Unix(), Model: model, Output: []respOutputItem{}}
	textIdx := -1 // 当前连续文本段对应的 message 项在 Output 中的下标；工具调用之后的文本另起一项
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			if textIdx < 0 {
				out.Output = append(out.Output, respOutputItem{
					Type: "message", ID: newID("msg_"), Status: "completed", Role: "assistant",
					Content: []respContentPart{{Type: "output_text", Annotations: []any{}}},
				})
				textIdx = len(out.Output) - 1
			}
			out.Output[textIdx].Content[0].Text += b.Text
		case "tool_use":
			textIdx = -1
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			out.Output = append(out.Output, respOutputItem{
				Type: "function_call", ID: newID("fc_"), Status: "completed", CallID: b.ID, Name: b.Name, Arguments: &args,
			})
		default:
			// thinking / redacted_thinking：§8.3 丢弃，不输出 reasoning 项
		}
	}
	out.Status, out.IncompleteDetails = statusFor(in.StopReason)
	out.Usage = in.Usage.toResponses()
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal responses body: %w", err)
	}
	return b, nil
}

// ---- 流式（§8.3 流式事件序列） ----

// responsesStream 把 Anthropic 事件流转换为 Responses 事件流。非并发安全（EventStream 约定）。
type responsesStream struct {
	in protocol.EventStream
	respEmitter

	usage  anthropicUsage
	stop   string
	blocks map[int]string // Anthropic content block index → 类型
}

func newResponsesStream(in protocol.EventStream, model string, now time.Time, newID idGen) *responsesStream {
	return &responsesStream{in: in, respEmitter: newRespEmitter(model, now, newID), blocks: map[int]string{}}
}

func (s *responsesStream) Close() error { return s.in.Close() }

// Next 返回下一个 Responses 事件。上游 error 事件或上游流中断返回 *protocol.AttemptError，
// 内核据此写出入站适配器的 StreamError（IN-R07）后结束。
func (s *responsesStream) Next() (sse.Event, error) {
	for {
		if ev, ok := s.pop(); ok {
			return ev, nil
		}
		if s.done {
			return sse.Event{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 上游在 message_stop 之前结束：流被截断，不能伪造 response.completed
				return sse.Event{}, streamFail(protocol.OutcomeNetworkError, errors.New("upstream stream ended before message_stop"))
			}
			return sse.Event{}, err
		}
		if err := s.handle(ev); err != nil {
			return sse.Event{}, err
		}
	}
}

func (s *responsesStream) handle(ev sse.Event) error {
	var data struct {
		Index        int             `json:"index"`
		Message      json.RawMessage `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
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
			return streamFail(protocol.OutcomeServerError, fmt.Errorf("decode anthropic event %q: %w", ev.Name, err))
		}
	}

	switch ev.Name {
	case "message_start":
		var msg struct {
			Usage anthropicUsage `json:"usage"`
		}
		if len(data.Message) > 0 {
			if err := json.Unmarshal(data.Message, &msg); err != nil {
				return streamFail(protocol.OutcomeServerError, fmt.Errorf("decode message_start: %w", err))
			}
		}
		s.usage.merge(msg.Usage)
		s.start()

	case "content_block_start":
		s.start()
		s.blocks[data.Index] = data.ContentBlock.Type
		switch data.ContentBlock.Type {
		case "text":
			s.textDelta(data.ContentBlock.Text)
		case "tool_use":
			s.openTool(data.Index, data.ContentBlock.ID, data.ContentBlock.Name)
		}

	case "content_block_delta":
		switch data.Delta.Type {
		case "text_delta":
			s.textDelta(data.Delta.Text)
		case "input_json_delta":
			s.toolArgs(data.Index, data.Delta.PartialJSON)
		default:
			// thinking_delta / signature_delta：§8.3 不输出
		}

	case "content_block_stop":
		if s.blocks[data.Index] == "tool_use" && s.tool != nil && s.tool.key == data.Index {
			s.closeTool()
		}
		// 文本块结束不关闭 message 项：文本在遇到工具调用或流结束时才结束（§8.3）

	case "message_delta":
		s.usage.merge(data.Usage)
		if data.Delta.StopReason != "" {
			s.stop = data.Delta.StopReason
		}

	case "message_stop":
		status, incomplete := statusFor(s.stop)
		s.finish(status, incomplete, s.usage.toResponses())

	case "error":
		msg := data.Error.Message
		if msg == "" {
			msg = "upstream stream error"
		}
		return streamFail(streamErrorKind(data.Error.Type), fmt.Errorf("anthropic stream error (%s): %s", data.Error.Type, msg))

	default:
		// ping 与未知事件：不输出
	}
	return nil
}

// streamErrorKind 与 Anthropic 出站适配器的 STR-05 分类一致（仅用于结果统计）。
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
