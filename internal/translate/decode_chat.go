package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

type chatDecoder struct{}

type chatInUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
}

func (u chatInUsage) cached() int64 {
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		return *u.PromptTokensDetails.CachedTokens
	}
	return u.PromptCacheHitTokens
}

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

// chatFinishToIR 把 Chat 的 finish_reason 映射为 IR 停止原因（spec/10 §6.3、§8.3）。
// insufficient_system_resource 表示上游推理被中断、输出不完整，视为上游错误（T44 G3）；
// 其他未列出的值与缺失 → StopEndTurn。
func chatFinishToIR(finish string) (ir.StopReason, error) {
	switch finish {
	case "insufficient_system_resource":
		return "", upstreamInvalid(fmt.Sprintf("upstream returned finish_reason %q (output incomplete)", finish))
	case "length":
		return ir.StopMaxTokens, nil
	case "tool_calls", "function_call":
		return ir.StopToolUse, nil
	case "content_filter":
		return ir.StopRefusal, nil
	default:
		return ir.StopEndTurn, nil
	}
}

func stripChatID(upstream string) string {
	if rest, ok := strings.CutPrefix(upstream, "chatcmpl-"); ok {
		return rest
	}
	return upstream
}

func (u chatInUsage) toIR() ir.TokenUsage {
	c := u.cached()
	return ir.TokenUsage{
		Input:      max(u.PromptTokens-c, 0),
		CacheRead:  c,
		CacheWrite: 0,
		Output:     u.CompletionTokens,
	}
}

func (d chatDecoder) Body(body []byte) (*ir.Response, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
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

	stop, err := chatFinishToIR(c.FinishReason)
	if err != nil {
		return nil, err
	}

	res := &ir.Response{
		ID:         stripChatID(in.ID),
		Model:      in.Model,
		StopReason: stop,
		Usage:      in.Usage.toIR(),
	}

	if m := c.Message.Content; m != nil && *m != "" {
		res.Parts = append(res.Parts, ir.TextPart{Text: *m})
	}
	if r := c.Message.Refusal; r != nil && *r != "" {
		res.Parts = append(res.Parts, ir.TextPart{Text: *r, IsRefusal: true})
	}
	for i, tc := range c.Message.ToolCalls {
		input, ok := toolInput(tc.Function.Arguments)
		if !ok {
			return nil, upstreamInvalid(fmt.Sprintf("upstream tool_calls[%d].function.arguments is not a JSON object", i))
		}
		res.Parts = append(res.Parts, ir.ToolCallPart{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: input,
		})
	}
	return res, nil
}

func (d chatDecoder) Stream(in protocol.EventStream) irStream {
	return newChatIRStream(in)
}

type chatIRStream struct {
	in    protocol.EventStream
	queue []ir.StreamEvent
	usage chatInUsage
	stop  string
	done  bool

	started   bool
	openTool  int
	hasOpen   bool
	toolMap   map[int]int // chunk index -> 0-based tool index
	seenTools map[int]bool
}

func newChatIRStream(in protocol.EventStream) *chatIRStream {
	return &chatIRStream{
		in:        in,
		toolMap:   make(map[int]int),
		seenTools: make(map[int]bool),
	}
}

func (s *chatIRStream) Close() error {
	return s.in.Close()
}

func (s *chatIRStream) Next() (ir.StreamEvent, error) {
	for len(s.queue) == 0 {
		if s.done {
			return ir.StreamEvent{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ir.StreamEvent{}, streamFail(protocol.OutcomeNetworkError, errors.New("upstream stream ended before [DONE]"))
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

func (s *chatIRStream) emit(ev ir.StreamEvent) {
	s.queue = append(s.queue, ev)
}

func (s *chatIRStream) handle(ev sse.Event) error {
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		if s.stop == "" {
			return streamFail(protocol.OutcomeServerError, errors.New("upstream sent [DONE] without finish_reason"))
		}
		if s.hasOpen {
			s.emit(ir.StreamEvent{
				Kind:      ir.EventToolEnd,
				ToolIndex: s.toolMap[s.openTool],
			})
			s.hasOpen = false
		}
		s.emit(ir.StreamEvent{Kind: ir.EventEnd})
		s.done = true
		return nil
	}

	var chunk struct {
		ID      string `json:"id"`
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

	if !s.started {
		s.started = true
		s.emit(ir.StreamEvent{
			Kind: ir.EventStart,
			ID:   stripChatID(chunk.ID),
		})
	}
	if chunk.Usage != nil {
		s.usage = *chunk.Usage
		s.emit(ir.StreamEvent{
			Kind:  ir.EventUsage,
			Usage: s.usage.toIR(),
		})
	}

	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			if s.hasOpen {
				s.emit(ir.StreamEvent{
					Kind:      ir.EventToolEnd,
					ToolIndex: s.toolMap[s.openTool],
				})
				s.hasOpen = false
			}
			s.emit(ir.StreamEvent{
				Kind: ir.EventText,
				Text: c.Delta.Content,
			})
		}
		if c.Delta.Refusal != "" {
			if s.hasOpen {
				s.emit(ir.StreamEvent{
					Kind:      ir.EventToolEnd,
					ToolIndex: s.toolMap[s.openTool],
				})
				s.hasOpen = false
			}
			s.emit(ir.StreamEvent{
				Kind:      ir.EventText,
				Text:      c.Delta.Refusal,
				IsRefusal: true,
			})
		}
		for _, tc := range c.Delta.ToolCalls {
			toolSeq, seen := s.toolMap[tc.Index]
			if !seen {
				if s.hasOpen {
					s.emit(ir.StreamEvent{
						Kind:      ir.EventToolEnd,
						ToolIndex: s.toolMap[s.openTool],
					})
					s.hasOpen = false
				}
				toolSeq = len(s.toolMap)
				s.toolMap[tc.Index] = toolSeq
				s.seenTools[tc.Index] = true
				s.openTool = tc.Index
				s.hasOpen = true
				s.emit(ir.StreamEvent{
					Kind:      ir.EventToolStart,
					ToolIndex: toolSeq,
					ToolID:    tc.ID,
					ToolName:  tc.Function.Name,
				})
			} else if !s.hasOpen || s.openTool != tc.Index {
				return streamFail(protocol.OutcomeServerError, fmt.Errorf("upstream interleaved arguments for tool call %d", tc.Index))
			}
			if tc.Function.Arguments != "" {
				s.emit(ir.StreamEvent{
					Kind:      ir.EventToolArgs,
					ToolIndex: toolSeq,
					Text:      tc.Function.Arguments,
				})
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			if s.hasOpen {
				s.emit(ir.StreamEvent{
					Kind:      ir.EventToolEnd,
					ToolIndex: s.toolMap[s.openTool],
				})
				s.hasOpen = false
			}
			stopReason, err := chatFinishToIR(*c.FinishReason)
			if err != nil {
				return streamFail(protocol.OutcomeServerError, err)
			}
			s.stop = *c.FinishReason
			s.emit(ir.StreamEvent{
				Kind: ir.EventStop,
				Stop: stopReason,
			})
		}
	}
	return nil
}
