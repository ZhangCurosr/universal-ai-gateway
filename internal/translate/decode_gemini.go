package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// geminiDecoder 把 Gemini 上游响应 / 流解码为 IR（spec/10 §10.5）。
// newID 用于给不带 id 的 functionCall 签发网关 ID（GID-01）。
type geminiDecoder struct {
	newID idGen
}

type geminiInUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
}

// toIR 实现 USE-G-02 口径（与 Gemini 出站适配器的计费口径一致）。
func (u geminiInUsage) toIR() ir.TokenUsage {
	return ir.TokenUsage{
		Input:     max(u.PromptTokenCount-u.CachedContentTokenCount, 0) + u.ToolUsePromptTokenCount,
		CacheRead: u.CachedContentTokenCount,
		Output:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
	}
}

type geminiInCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiInCandidate struct {
	Content struct {
		Parts []struct {
			Text         *string       `json:"text"`
			Thought      bool          `json:"thought"`
			FunctionCall *geminiInCall `json:"functionCall"`
		} `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

type geminiInChunk struct {
	ResponseID     string              `json:"responseId"`
	Candidates     []geminiInCandidate `json:"candidates"`
	UsageMetadata  *geminiInUsage      `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// geminiFinishToIR 实现 §10.5 的 finishReason 表；返回 error 表示上游故障。
func geminiFinishToIR(reason string, hasCall bool) (ir.StopReason, error) {
	switch reason {
	case "STOP":
		if hasCall {
			return ir.StopToolUse, nil
		}
		return ir.StopEndTurn, nil
	case "MAX_TOKENS":
		return ir.StopMaxTokens, nil
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return ir.StopRefusal, nil
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "MISSING_THOUGHT_SIGNATURE", "MALFORMED_RESPONSE":
		return "", upstreamInvalid(fmt.Sprintf("upstream returned finishReason %q", reason))
	default: // 缺失、FINISH_REASON_UNSPECIFIED、OTHER、LANGUAGE 与未列出的值（§9.2 第 3 条）
		if hasCall {
			return ir.StopToolUse, nil
		}
		return ir.StopEndTurn, nil
	}
}

// geminiArgs 校验 functionCall.args：缺失 → {}；不是对象 → false。
func geminiArgs(raw json.RawMessage) (json.RawMessage, bool) {
	if !present(raw) {
		return json.RawMessage(`{}`), true
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil || buf.Len() == 0 || buf.Bytes()[0] != '{' {
		return nil, false
	}
	return buf.Bytes(), true
}

func (d geminiDecoder) callID(id string) string {
	if id != "" {
		return id
	}
	return d.newID("call_")
}

func (d geminiDecoder) Body(body []byte) (*ir.Response, error) {
	var in geminiInChunk
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode gemini response: %w", err)
	}
	res := &ir.Response{ID: in.ResponseID}
	if in.UsageMetadata != nil {
		res.Usage = in.UsageMetadata.toIR()
	}
	if len(in.Candidates) == 0 && in.PromptFeedback != nil && in.PromptFeedback.BlockReason != "" {
		res.StopReason = ir.StopRefusal
		return res, nil
	}
	if len(in.Candidates) != 1 {
		return nil, upstreamInvalid(fmt.Sprintf("upstream returned %d candidates, want exactly 1", len(in.Candidates)))
	}
	c := in.Candidates[0]
	hasCall := false
	for i, p := range c.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			args, ok := geminiArgs(p.FunctionCall.Args)
			if !ok {
				return nil, upstreamInvalid(fmt.Sprintf("upstream parts[%d].functionCall.args is not a JSON object", i))
			}
			hasCall = true
			res.Parts = append(res.Parts, ir.ToolCallPart{ID: d.callID(p.FunctionCall.ID), Name: p.FunctionCall.Name, Arguments: args})
		case p.Text != nil && *p.Text != "":
			if p.Thought {
				res.Parts = append(res.Parts, ir.ThinkingPart{Vendor: ir.VendorGemini, Text: *p.Text})
			} else {
				res.Parts = append(res.Parts, ir.TextPart{Text: *p.Text})
			}
		}
	}
	stop, err := geminiFinishToIR(c.FinishReason, hasCall)
	if err != nil {
		return nil, err
	}
	res.StopReason = stop
	return res, nil
}

func (d geminiDecoder) Stream(in protocol.EventStream) irStream {
	return &geminiIRStream{in: in, dec: d}
}

type geminiIRStream struct {
	in        protocol.EventStream
	dec       geminiDecoder
	queue     []ir.StreamEvent
	started   bool
	sawFinish bool
	hasCall   bool
	tools     int
	done      bool
}

func (s *geminiIRStream) Close() error { return s.in.Close() }

func (s *geminiIRStream) Next() (ir.StreamEvent, error) {
	for len(s.queue) == 0 {
		if s.done {
			return ir.StreamEvent{}, io.EOF
		}
		ev, err := s.in.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return ir.StreamEvent{}, err
			}
			if !s.sawFinish {
				return ir.StreamEvent{}, streamFail(protocol.OutcomeNetworkError, errors.New("upstream stream ended before finishReason"))
			}
			s.queue = append(s.queue, ir.StreamEvent{Kind: ir.EventEnd})
			s.done = true
			continue
		}
		if err := s.handle(ev); err != nil {
			return ir.StreamEvent{}, err
		}
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, nil
}

func (s *geminiIRStream) emit(ev ir.StreamEvent) { s.queue = append(s.queue, ev) }

func (s *geminiIRStream) handle(ev sse.Event) error {
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil
	}
	var chunk geminiInChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return streamFail(protocol.OutcomeServerError, fmt.Errorf("decode gemini chunk: %w", err))
	}
	if e := chunk.Error; e != nil { // STR-G-01
		kind := protocol.OutcomeServerError
		switch {
		case e.Code == 429 || e.Status == "RESOURCE_EXHAUSTED":
			kind = protocol.OutcomeRateLimited
		case e.Code == 503 || e.Status == "UNAVAILABLE":
			kind = protocol.OutcomeOverloaded
		}
		return streamFail(kind, fmt.Errorf("gemini stream error (%s): %s", e.Status, e.Message))
	}
	if !s.started {
		s.started = true
		s.emit(ir.StreamEvent{Kind: ir.EventStart, ID: chunk.ResponseID})
	}
	if len(chunk.Candidates) == 0 && chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
		s.sawFinish = true
		s.emit(ir.StreamEvent{Kind: ir.EventStop, Stop: ir.StopRefusal})
	}
	var finish string
	if len(chunk.Candidates) > 0 {
		c := chunk.Candidates[0]
		for i, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				args, ok := geminiArgs(p.FunctionCall.Args)
				if !ok {
					return streamFail(protocol.OutcomeServerError, fmt.Errorf("upstream parts[%d].functionCall.args is not a JSON object", i))
				}
				s.hasCall = true
				idx := s.tools
				s.tools++
				s.emit(ir.StreamEvent{Kind: ir.EventToolStart, ToolIndex: idx, ToolID: s.dec.callID(p.FunctionCall.ID), ToolName: p.FunctionCall.Name})
				s.emit(ir.StreamEvent{Kind: ir.EventToolArgs, ToolIndex: idx, Text: string(args)})
				s.emit(ir.StreamEvent{Kind: ir.EventToolEnd, ToolIndex: idx})
			case p.Text != nil && *p.Text != "":
				if p.Thought {
					s.emit(ir.StreamEvent{Kind: ir.EventThinking, Text: *p.Text})
				} else {
					s.emit(ir.StreamEvent{Kind: ir.EventText, Text: *p.Text})
				}
			}
		}
		finish = c.FinishReason
	}
	if chunk.UsageMetadata != nil {
		s.emit(ir.StreamEvent{Kind: ir.EventUsage, Usage: chunk.UsageMetadata.toIR()})
	}
	if finish != "" {
		stop, err := geminiFinishToIR(finish, s.hasCall)
		if err != nil {
			return streamFail(protocol.OutcomeServerError, err)
		}
		s.sawFinish = true
		s.emit(ir.StreamEvent{Kind: ir.EventStop, Stop: stop})
	}
	return nil
}
