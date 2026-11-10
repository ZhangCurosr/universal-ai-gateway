package translate

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

func TestGeminiFinishReasons(t *testing.T) {
	tests := []struct {
		reason  string
		hasCall bool
		want    ir.StopReason
		fault   bool
	}{
		{"STOP", false, ir.StopEndTurn, false},
		{"STOP", true, ir.StopToolUse, false},
		{"", false, ir.StopEndTurn, false},
		{"FINISH_REASON_UNSPECIFIED", false, ir.StopEndTurn, false},
		{"OTHER", false, ir.StopEndTurn, false},
		{"LANGUAGE", false, ir.StopEndTurn, false},
		{"SOMETHING_NEW", false, ir.StopEndTurn, false},
		{"MAX_TOKENS", false, ir.StopMaxTokens, false},
		{"SAFETY", false, ir.StopRefusal, false},
		{"RECITATION", false, ir.StopRefusal, false},
		{"BLOCKLIST", false, ir.StopRefusal, false},
		{"PROHIBITED_CONTENT", false, ir.StopRefusal, false},
		{"SPII", false, ir.StopRefusal, false},
		{"IMAGE_SAFETY", false, ir.StopRefusal, false},
		{"MALFORMED_FUNCTION_CALL", false, "", true},
		{"UNEXPECTED_TOOL_CALL", false, "", true},
		{"TOO_MANY_TOOL_CALLS", false, "", true},
		{"MISSING_THOUGHT_SIGNATURE", false, "", true},
		{"MALFORMED_RESPONSE", false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			got, err := geminiFinishToIR(tt.reason, tt.hasCall)
			if tt.fault {
				var ae *apperr.Error
				if !errors.As(err, &ae) || ae.Code != apperr.CodeUpstreamError {
					t.Fatalf("geminiFinishToIR(%q) error = %v, want upstream_error", tt.reason, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("geminiFinishToIR(%q, %v) = %q, %v; want %q", tt.reason, tt.hasCall, got, err, tt.want)
			}
		})
	}
}

func TestGeminiDecoderBody(t *testing.T) {
	dec := geminiDecoder{newID: seqIDs()}
	t.Run("blockReason", func(t *testing.T) {
		res, err := dec.Body([]byte(`{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":3}}`))
		if err != nil || res.StopReason != ir.StopRefusal || len(res.Parts) != 0 || res.Usage.Input != 3 {
			t.Fatalf("Body() = %+v, %v", res, err)
		}
	})
	t.Run("MALFORMED_FUNCTION_CALL is 502", func(t *testing.T) {
		_, err := dec.Body([]byte(`{"candidates":[{"content":{"parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`))
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != apperr.CodeUpstreamError {
			t.Fatalf("error = %v, want upstream_error", err)
		}
	})
	t.Run("two candidates", func(t *testing.T) {
		_, err := dec.Body([]byte(`{"candidates":[{},{}]}`))
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != apperr.CodeUpstreamError {
			t.Fatalf("error = %v, want upstream_error", err)
		}
	})
	t.Run("args not object", func(t *testing.T) {
		_, err := dec.Body([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":"x"}}]},"finishReason":"STOP"}]}`))
		if err == nil {
			t.Fatal("want error for non-object args")
		}
	})
	t.Run("usage mapping", func(t *testing.T) {
		res, err := dec.Body([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":20,"cachedContentTokenCount":8,"toolUsePromptTokenCount":4,"candidatesTokenCount":5,"thoughtsTokenCount":3}}`))
		want := ir.TokenUsage{Input: 16, CacheRead: 8, Output: 8}
		if err != nil || res.Usage != want {
			t.Fatalf("usage = %+v, %v; want %+v", res.Usage, err, want)
		}
	})
}

func geminiSSE(chunks ...string) *sliceStream {
	evs := make([]sse.Event, len(chunks))
	for i, c := range chunks {
		evs[i] = sse.Event{Data: []byte(c)}
	}
	return &sliceStream{evs: evs}
}

func drainIR(s irStream) ([]ir.StreamEvent, error) {
	var out []ir.StreamEvent
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func wantOutcome(t *testing.T, err error, kind protocol.OutcomeKind) {
	t.Helper()
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != kind {
		t.Fatalf("error = %v, want AttemptError with outcome %v", err, kind)
	}
}

func TestGeminiDecoderStream(t *testing.T) {
	dec := geminiDecoder{newID: seqIDs()}
	t.Run("EOF without finishReason", func(t *testing.T) {
		_, err := drainIR(dec.Stream(geminiSSE(`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`)))
		wantOutcome(t, err, protocol.OutcomeNetworkError)
	})
	t.Run("error chunk 429", func(t *testing.T) {
		_, err := drainIR(dec.Stream(geminiSSE(`{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`)))
		wantOutcome(t, err, protocol.OutcomeRateLimited)
	})
	t.Run("error chunk 503", func(t *testing.T) {
		_, err := drainIR(dec.Stream(geminiSSE(`{"error":{"code":503,"message":"busy","status":"UNAVAILABLE"}}`)))
		wantOutcome(t, err, protocol.OutcomeOverloaded)
	})
	t.Run("error chunk 500", func(t *testing.T) {
		_, err := drainIR(dec.Stream(geminiSSE(`{"error":{"code":500,"message":"x","status":"INTERNAL"}}`)))
		wantOutcome(t, err, protocol.OutcomeServerError)
	})
	t.Run("MALFORMED_FUNCTION_CALL is a stream error", func(t *testing.T) {
		_, err := drainIR(dec.Stream(geminiSSE(`{"candidates":[{"content":{"parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`)))
		wantOutcome(t, err, protocol.OutcomeServerError)
	})
	t.Run("tool call issues gateway id", func(t *testing.T) {
		evs, err := drainIR(dec.Stream(geminiSSE(`{"responseId":"r","candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]},"finishReason":"STOP"}]}`)))
		if err != nil {
			t.Fatal(err)
		}
		kinds := []ir.StreamEventKind{ir.EventStart, ir.EventToolStart, ir.EventToolArgs, ir.EventToolEnd, ir.EventStop, ir.EventEnd}
		if len(evs) != len(kinds) {
			t.Fatalf("events = %+v", evs)
		}
		for i, k := range kinds {
			if evs[i].Kind != k {
				t.Fatalf("event %d kind = %v, want %v (%+v)", i, evs[i].Kind, k, evs)
			}
		}
		if !strings.HasPrefix(evs[1].ToolID, "call_") || evs[2].Text != `{}` || evs[4].Stop != ir.StopToolUse {
			t.Fatalf("tool events = %+v", evs)
		}
	})
	t.Run("blockReason", func(t *testing.T) {
		evs, err := drainIR(dec.Stream(geminiSSE(`{"promptFeedback":{"blockReason":"OTHER"}}`)))
		if err != nil || evs[len(evs)-2].Stop != ir.StopRefusal || evs[len(evs)-1].Kind != ir.EventEnd {
			t.Fatalf("events = %+v, %v", evs, err)
		}
	})
}

// irSlice 是测试用的 IR 事件源。
type irSlice struct {
	evs []ir.StreamEvent
	end error
}

func (s *irSlice) Next() (ir.StreamEvent, error) {
	if len(s.evs) == 0 {
		if s.end != nil {
			return ir.StreamEvent{}, s.end
		}
		return ir.StreamEvent{}, io.EOF
	}
	ev := s.evs[0]
	s.evs = s.evs[1:]
	return ev, nil
}
func (s *irSlice) Close() error { return nil }

func drainEncoded(s protocol.EventStream) ([]sse.Event, error) {
	var out []sse.Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func TestGeminiEncoderStream(t *testing.T) {
	enc := newGeminiEncoder(seqIDs())
	tool := func(args ...string) []ir.StreamEvent {
		evs := []ir.StreamEvent{{Kind: ir.EventStart, ID: "r1"}, {Kind: ir.EventToolStart, ToolIndex: 0, ToolName: "f"}}
		for _, a := range args {
			evs = append(evs, ir.StreamEvent{Kind: ir.EventToolArgs, ToolIndex: 0, Text: a})
		}
		return append(evs, ir.StreamEvent{Kind: ir.EventStop, Stop: ir.StopToolUse}, ir.StreamEvent{Kind: ir.EventEnd})
	}
	t.Run("GE-02 buffers fragments into one functionCall chunk", func(t *testing.T) {
		got, err := drainEncoded(enc.Stream(&irSlice{evs: tool(`{"a"`, `:1}`)}, geminiClientEnv))
		if err != nil {
			t.Fatal(err)
		}
		checkGeminiChunks(t, got)
		if len(got) != 2 || !strings.Contains(string(got[0].Data), `"functionCall":{"id":"call_1","name":"f","args":{"a":1}}`) {
			t.Fatalf("chunks = %s", got)
		}
		if !strings.Contains(string(got[1].Data), `"finishReason":"STOP"`) {
			t.Fatalf("final chunk = %s", got[1].Data)
		}
	})
	t.Run("GE-02 non-object args", func(t *testing.T) {
		_, err := drainEncoded(enc.Stream(&irSlice{evs: tool(`[1,2]`)}, geminiClientEnv))
		wantOutcome(t, err, protocol.OutcomeServerError)
	})
	t.Run("GE-02 over 1 MiB", func(t *testing.T) {
		big := `{"a":"` + strings.Repeat("x", maxGeminiToolArgs) + `"}`
		_, err := drainEncoded(enc.Stream(&irSlice{evs: tool(big)}, geminiClientEnv))
		wantOutcome(t, err, protocol.OutcomeServerError)
	})
	t.Run("refusal maps to SAFETY", func(t *testing.T) {
		got, err := drainEncoded(enc.Stream(&irSlice{evs: []ir.StreamEvent{
			{Kind: ir.EventStart}, {Kind: ir.EventText, Text: "no", IsRefusal: true}, {Kind: ir.EventStop, Stop: ir.StopEndTurn}, {Kind: ir.EventEnd},
		}}, geminiClientEnv))
		if err != nil || !strings.Contains(string(got[len(got)-1].Data), `"finishReason":"SAFETY"`) {
			t.Fatalf("chunks = %s, %v", got, err)
		}
	})
	t.Run("upstream error passes through", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := drainEncoded(enc.Stream(&irSlice{evs: []ir.StreamEvent{{Kind: ir.EventStart}}, end: boom}, geminiClientEnv))
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want boom", err)
		}
	})
}
