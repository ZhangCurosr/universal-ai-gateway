package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// maxGeminiToolArgs 是 Gemini Encoder 拼接单个工具调用参数的缓冲上限（GE-02）。
const maxGeminiToolArgs = 1 << 20

// geminiEncoder 把 IR 编码为 Gemini 客户端响应 / 流（spec/10 §10.6）。
type geminiEncoder struct {
	newID idGen
}

func newGeminiEncoder(newID idGen) geminiEncoder { return geminiEncoder{newID: newID} }

type geminiOutUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
}

func tokenUsageToGemini(u ir.TokenUsage) *geminiOutUsage {
	prompt := u.Input + u.CacheRead + u.CacheWrite
	return &geminiOutUsage{
		PromptTokenCount:        prompt,
		CandidatesTokenCount:    u.Output,
		TotalTokenCount:         prompt + u.Output,
		CachedContentTokenCount: u.CacheRead,
	}
}

func geminiFinishReason(stop ir.StopReason, refusal bool) string {
	switch {
	case refusal || stop == ir.StopRefusal:
		return "SAFETY"
	case stop == ir.StopMaxTokens:
		return "MAX_TOKENS"
	default: // StopEndTurn、StopToolUse、StopSequence
		return "STOP"
	}
}

type geminiOutCandidate struct {
	Content      geminiOutContent `json:"content"`
	FinishReason string           `json:"finishReason,omitempty"`
	Index        int              `json:"index"`
}

type geminiOutChunk struct {
	Candidates    []geminiOutCandidate `json:"candidates"`
	UsageMetadata *geminiOutUsage      `json:"usageMetadata,omitempty"`
	ModelVersion  string               `json:"modelVersion"`
	ResponseID    string               `json:"responseId"`
}

func (e geminiEncoder) responseID(upstream string) string {
	if upstream != "" {
		return upstream
	}
	return e.newID("")
}

func (e geminiEncoder) Body(resp *ir.Response, env *protocol.Envelope) ([]byte, error) {
	parts := []geminiOutPart{}
	refusal := false
	for _, p := range resp.Parts {
		switch v := p.(type) {
		case ir.TextPart:
			refusal = refusal || v.IsRefusal
			parts = append(parts, geminiOutPart{Text: strPtr(v.Text)})
		case ir.ToolCallPart:
			args := v.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			parts = append(parts, geminiOutPart{FunctionCall: &geminiOutCall{ID: v.ID, Name: v.Name, Args: args}})
		case ir.ThinkingPart:
			// 丢弃（与 T43 B4 一致：只有 Chat 客户端输出思考内容）
		}
	}
	out := geminiOutChunk{
		Candidates: []geminiOutCandidate{{
			Content:      geminiOutContent{Role: "model", Parts: parts},
			FinishReason: geminiFinishReason(resp.StopReason, refusal),
		}},
		UsageMetadata: tokenUsageToGemini(resp.Usage),
		ModelVersion:  env.Model,
		ResponseID:    e.responseID(resp.ID),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini body: %w", err)
	}
	return b, nil
}

func (e geminiEncoder) Stream(in irStream, env *protocol.Envelope) protocol.EventStream {
	return &geminiEncoderStream{in: in, enc: e, model: env.Model}
}

type geminiEncoderStream struct {
	in    irStream
	enc   geminiEncoder
	model string

	id      string
	queue   []sse.Event
	usage   ir.TokenUsage
	stop    ir.StopReason
	refusal bool
	done    bool

	hasTool  bool
	toolID   string
	toolName string
	toolArgs []byte
}

func (s *geminiEncoderStream) Close() error { return s.in.Close() }

func (s *geminiEncoderStream) Next() (sse.Event, error) {
	for len(s.queue) == 0 {
		if s.done {
			return sse.Event{}, io.EOF // GE-04：没有结束标记
		}
		ev, err := s.in.Next()
		if err != nil {
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

func (s *geminiEncoderStream) ensureID(upstream string) {
	if s.id == "" {
		s.id = s.enc.responseID(upstream)
	}
}

// chunk 输出一个 GenerateContentResponse 片段（GE-01、GE-05）。
func (s *geminiEncoderStream) chunk(parts []geminiOutPart, finish string, usage *geminiOutUsage) {
	s.ensureID("")
	if parts == nil {
		parts = []geminiOutPart{}
	}
	c := geminiOutChunk{
		Candidates:    []geminiOutCandidate{{Content: geminiOutContent{Role: "model", Parts: parts}, FinishReason: finish}},
		UsageMetadata: usage,
		ModelVersion:  s.model,
		ResponseID:    s.id,
	}
	b, err := json.Marshal(c)
	if err != nil { // 只含字符串、整数与已校验的 JSON 对象，不会失败
		b = []byte(`{"candidates":[]}`)
	}
	s.queue = append(s.queue, sse.Event{Data: b})
}

// flushTool 输出缓冲中的工具调用（GE-02）。
func (s *geminiEncoderStream) flushTool() error {
	if !s.hasTool {
		return nil
	}
	s.hasTool = false
	args, ok := geminiArgs(s.toolArgs)
	if !ok {
		return streamFail(protocol.OutcomeServerError, fmt.Errorf("tool call %q arguments are not a JSON object", s.toolName))
	}
	s.chunk([]geminiOutPart{{FunctionCall: &geminiOutCall{ID: s.toolID, Name: s.toolName, Args: args}}}, "", nil)
	s.toolArgs = nil
	return nil
}

func (s *geminiEncoderStream) handle(ev ir.StreamEvent) error {
	switch ev.Kind {
	case ir.EventStart:
		s.ensureID(ev.ID)
	case ir.EventText:
		if err := s.flushTool(); err != nil {
			return err
		}
		s.refusal = s.refusal || ev.IsRefusal
		s.chunk([]geminiOutPart{{Text: strPtr(ev.Text)}}, "", nil)
	case ir.EventThinking:
		// 丢弃（§10.6）
	case ir.EventToolStart:
		if err := s.flushTool(); err != nil {
			return err
		}
		s.hasTool, s.toolID, s.toolName, s.toolArgs = true, ev.ToolID, ev.ToolName, nil
		if s.toolID == "" {
			s.toolID = s.enc.newID("call_")
		}
	case ir.EventToolArgs:
		if !s.hasTool {
			return streamFail(protocol.OutcomeServerError, errors.New("tool arguments without a tool call"))
		}
		if len(s.toolArgs)+len(ev.Text) > maxGeminiToolArgs {
			return streamFail(protocol.OutcomeServerError, fmt.Errorf("tool call %q arguments exceed %d bytes", s.toolName, maxGeminiToolArgs))
		}
		s.toolArgs = append(s.toolArgs, ev.Text...)
	case ir.EventToolEnd:
		return s.flushTool()
	case ir.EventUsage:
		s.usage = ev.Usage
	case ir.EventStop:
		if err := s.flushTool(); err != nil {
			return err
		}
		s.stop = ev.Stop
	case ir.EventEnd:
		if err := s.flushTool(); err != nil {
			return err
		}
		s.chunk(nil, geminiFinishReason(s.stop, s.refusal), tokenUsageToGemini(s.usage)) // GE-03
		s.done = true
	}
	return nil
}
