package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

type responsesEncoder struct {
	now   time.Time
	newID idGen
}

func newResponsesEncoder(now time.Time, newID idGen) responsesEncoder {
	return responsesEncoder{now: now, newID: newID}
}

type idGen func(prefix string) string

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

type respObject struct {
	ID                string           `json:"id"`
	Object            string           `json:"object"`
	CreatedAt         int64            `json:"created_at"`
	Status            string           `json:"status"`
	Model             string           `json:"model"`
	Output            []respOutputItem `json:"output"`
	Usage             *respUsage       `json:"usage,omitempty"`
	IncompleteDetails *respIncomplete  `json:"incomplete_details"`
	Store             bool             `json:"store"`
}

func statusFor(stopReason string) (string, *respIncomplete) {
	switch stopReason {
	case "max_tokens":
		return "incomplete", &respIncomplete{Reason: "max_output_tokens"}
	case "refusal":
		return "incomplete", &respIncomplete{Reason: "content_filter"}
	default:
		return "completed", nil
	}
}

func chatStatusFor(finish string) (string, *respIncomplete) {
	if finish == "length" {
		return "incomplete", &respIncomplete{Reason: "max_output_tokens"}
	}
	return "completed", nil
}

func anthropicToResponsesBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	dec := anthropicDecoder{}
	enc := newResponsesEncoder(now, newID)
	resp, err := dec.Body(body)
	if err != nil {
		return nil, err
	}
	return enc.Body(resp, &protocol.Envelope{Model: model})
}

func chatToResponsesBody(body []byte, model string, now time.Time, newID idGen) ([]byte, error) {
	dec := chatDecoder{}
	enc := newResponsesEncoder(now, newID)
	resp, err := dec.Body(body)
	if err != nil {
		return nil, err
	}
	return enc.Body(resp, &protocol.Envelope{Model: model})
}

func tokenUsageToResponses(u ir.TokenUsage) *respUsage {
	input := u.Input + u.CacheRead + u.CacheWrite
	r := &respUsage{
		InputTokens:  input,
		OutputTokens: u.Output,
		TotalTokens:  input + u.Output,
	}
	r.InputTokensDetails.CachedTokens = u.CacheRead
	return r
}

func mapStopReasonToResponses(r ir.StopReason) (string, *respIncomplete) {
	switch r {
	case ir.StopMaxTokens:
		return "incomplete", &respIncomplete{Reason: "max_output_tokens"}
	case ir.StopRefusal:
		return "incomplete", &respIncomplete{Reason: "content_filter"}
	default:
		return "completed", nil
	}
}

func (e responsesEncoder) Body(resp *ir.Response, env *protocol.Envelope) ([]byte, error) {
	now := e.now
	status, incomplete := mapStopReasonToResponses(resp.StopReason)

	out := respObject{
		ID:                e.newID("resp_"),
		Object:            "response",
		CreatedAt:         now.Unix(),
		Status:            status,
		Model:             env.Model,
		Output:            []respOutputItem{},
		Usage:             tokenUsageToResponses(resp.Usage),
		IncompleteDetails: incomplete,
		Store:             false,
	}

	textIdx := -1
	for _, p := range resp.Parts {
		switch part := p.(type) {
		case ir.TextPart:
			if part.Text == "" {
				continue
			}
			if textIdx < 0 {
				out.Output = append(out.Output, respOutputItem{
					Type:    "message",
					ID:      e.newID("msg_"),
					Status:  "completed",
					Role:    "assistant",
					Content: []respContentPart{{Type: "output_text", Annotations: []any{}}},
				})
				textIdx = len(out.Output) - 1
			}
			out.Output[textIdx].Content[0].Text += part.Text
		case ir.ToolCallPart:
			textIdx = -1
			args := "{}"
			if len(part.Arguments) > 0 {
				args = string(part.Arguments)
			}
			out.Output = append(out.Output, respOutputItem{
				Type:      "function_call",
				ID:        e.newID("fc_"),
				Status:    "completed",
				CallID:    part.ID,
				Name:      part.Name,
				Arguments: &args,
			})
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal responses body: %w", err)
	}
	return b, nil
}

func (e responsesEncoder) Stream(in irStream, env *protocol.Envelope) protocol.EventStream {
	return newResponsesEncoderStream(in, env.Model, e.now, e.newID)
}

type responsesEncoderStream struct {
	in      irStream
	emitter respEmitter
	usage   ir.TokenUsage
	status  string
	incomp  *respIncomplete
	done    bool
}

func newResponsesEncoderStream(in irStream, model string, now time.Time, newID idGen) *responsesEncoderStream {
	return &responsesEncoderStream{
		in:      in,
		emitter: newRespEmitter(model, now, newID),
		status:  "completed",
	}
}

func (s *responsesEncoderStream) Close() error {
	return s.in.Close()
}

func (s *responsesEncoderStream) Next() (sse.Event, error) {
	for {
		if ev, ok := s.emitter.pop(); ok {
			return ev, nil
		}
		if s.done {
			return sse.Event{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			return sse.Event{}, err
		}
		s.handle(ev)
	}
}

func (s *responsesEncoderStream) handle(ev ir.StreamEvent) {
	switch ev.Kind {
	case ir.EventStart:
		s.emitter.start()
		if ev.Usage.Input != 0 || ev.Usage.Output != 0 || ev.Usage.CacheRead != 0 || ev.Usage.CacheWrite != 0 {
			s.usage = ev.Usage
		}

	case ir.EventText:
		s.emitter.textDelta(ev.Text)

	case ir.EventThinking:
		// Responses 客户端不输出思考内容（B4）

	case ir.EventToolStart:
		s.emitter.openTool(ev.ToolIndex, ev.ToolID, ev.ToolName)

	case ir.EventToolArgs:
		s.emitter.toolArgs(ev.ToolIndex, ev.Text)

	case ir.EventToolEnd:
		s.emitter.closeTool()

	case ir.EventStop:
		s.status, s.incomp = mapStopReasonToResponses(ev.Stop)

	case ir.EventUsage:
		s.usage = ev.Usage

	case ir.EventEnd:
		s.emitter.finish(s.status, s.incomp, tokenUsageToResponses(s.usage))
		s.done = true
	}
}
