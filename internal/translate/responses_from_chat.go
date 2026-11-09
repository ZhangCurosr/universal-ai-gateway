package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// 本文件实现 spec/10 §8.3「自 Chat 上游」：OpenAI Chat 响应/流 → OpenAI Responses 响应/流（T27）。

// toResponses 实现 §8.3 自 Chat 的 usage 映射：input_tokens = prompt_tokens（已含缓存）。
func (u chatInUsage) toResponses() *respUsage {
	r := &respUsage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	r.InputTokensDetails.CachedTokens = u.cached()
	r.TotalTokens = r.InputTokens + r.OutputTokens
	return r
}

// chatStatusFor：finish_reason length → incomplete，其余 → completed（§8.3）。
func chatStatusFor(finish string) (string, *respIncomplete) {
	if finish == "length" {
		return "incomplete", &respIncomplete{Reason: "max_output_tokens"}
	}
	return "completed", nil
}

// chatToResponsesBody 转换非流式 Chat 响应体（§8.3 非流式）。
//
// spec/10 §8.3：Chat 上游的 refusal 文本作为 output_text 输出（与 §6.3 把拒绝文本
// 作为 text 块的做法一致），不生成 Responses 的 refusal 内容部件。
func chatToResponsesBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	var in struct {
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
				// reasoning_content：§8.3 丢弃
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

	out := respObject{ID: newID("resp_"), Object: "response", CreatedAt: now.Unix(), Model: model, Output: []respOutputItem{}}
	var text string
	for _, p := range []*string{c.Message.Content, c.Message.Refusal} {
		if p != nil {
			text += *p
		}
	}
	if text != "" {
		out.Output = append(out.Output, respOutputItem{
			Type: "message", ID: newID("msg_"), Status: "completed", Role: "assistant",
			Content: []respContentPart{{Type: "output_text", Text: text, Annotations: []any{}}},
		})
	}
	for i, tc := range c.Message.ToolCalls {
		input, ok := toolInput(tc.Function.Arguments)
		if !ok {
			return nil, upstreamInvalid(fmt.Sprintf("upstream tool_calls[%d].function.arguments is not a JSON object", i))
		}
		args := string(input)
		out.Output = append(out.Output, respOutputItem{
			Type: "function_call", ID: newID("fc_"), Status: "completed", CallID: tc.ID, Name: tc.Function.Name, Arguments: &args,
		})
	}
	out.Status, out.IncompleteDetails = chatStatusFor(c.FinishReason)
	out.Usage = in.Usage.toResponses()
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal responses body: %w", err)
	}
	return b, nil
}

// ---- 流式 ----

// chatResponsesStream 把 Chat chunk 流转换为 Responses 事件流。非并发安全（EventStream 约定）。
type chatResponsesStream struct {
	in protocol.EventStream
	respEmitter

	usage     chatInUsage
	stop      string // 已收到的 finish_reason；空 = 尚未收到
	seenTools map[int]bool
}

func newChatResponsesStream(in protocol.EventStream, model string, now time.Time, newID idGen) *chatResponsesStream {
	return &chatResponsesStream{in: in, respEmitter: newRespEmitter(model, now, newID), seenTools: map[int]bool{}}
}

func (s *chatResponsesStream) Close() error { return s.in.Close() }

// Next 返回下一个 Responses 事件。上游错误 chunk、流截断、在 finish_reason 之前收到 [DONE] 或工具参数交错
// 返回 *protocol.AttemptError，内核据此写出入站适配器的 StreamError 后结束。
func (s *chatResponsesStream) Next() (sse.Event, error) {
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
				return sse.Event{}, streamFail(protocol.OutcomeNetworkError, errors.New("upstream stream ended before [DONE]"))
			}
			return sse.Event{}, err
		}
		if err := s.handle(ev); err != nil {
			return sse.Event{}, err
		}
	}
}

func (s *chatResponsesStream) handle(ev sse.Event) error {
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		if s.stop == "" {
			return streamFail(protocol.OutcomeServerError, errors.New("upstream sent [DONE] without finish_reason"))
		}
		status, incomplete := chatStatusFor(s.stop)
		s.finish(status, incomplete, s.usage.toResponses())
		return nil
	}

	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				// reasoning_content：§8.3 不输出
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
	s.start()
	if chunk.Usage != nil {
		s.usage = *chunk.Usage
	}
	for _, c := range chunk.Choices {
		s.textDelta(c.Delta.Content)
		s.textDelta(c.Delta.Refusal) // spec/10 §8.3：拒绝文本作为 output_text，见 chatToResponsesBody
		for _, tc := range c.Delta.ToolCalls {
			if !s.seenTools[tc.Index] {
				s.seenTools[tc.Index] = true
				s.openTool(tc.Index, tc.ID, tc.Function.Name)
			}
			if !s.toolArgs(tc.Index, tc.Function.Arguments) {
				return streamFail(protocol.OutcomeServerError, fmt.Errorf("upstream interleaved arguments for tool call %d", tc.Index))
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			s.stop = *c.FinishReason
		}
	}
	return nil
}
