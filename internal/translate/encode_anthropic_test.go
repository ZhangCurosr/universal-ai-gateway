package translate

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func mockIDGen(prefix string) string {
	return prefix + "mock_id"
}

func TestAnthropicEncoderBody(t *testing.T) {
	enc := newAnthropicEncoder(mockIDGen)
	resp := &ir.Response{
		ID:         "orig_123",
		Model:      "test-model",
		StopReason: ir.StopEndTurn,
		Parts: []ir.Part{
			ir.TextPart{Text: "Hello there"},
		},
		Usage: ir.TokenUsage{Input: 10, Output: 5},
	}
	body, err := enc.Body(resp, &protocol.Envelope{Model: "claude-3-5"})
	if err != nil {
		t.Fatal(err)
	}
	var out anthropicOutMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "msg_orig_123" {
		t.Errorf("expected msg_orig_123, got %s", out.ID)
	}
	if out.Model != "claude-3-5" {
		t.Errorf("expected claude-3-5, got %s", out.Model)
	}
	c0, ok := out.Content[0].(map[string]any)
	if !ok || len(out.Content) != 1 || c0["text"] != "Hello there" {
		t.Errorf("unexpected content: %+v", out.Content)
	}
}

type sliceIRStream struct {
	events []ir.StreamEvent
	idx    int
}

func (s *sliceIRStream) Next() (ir.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return ir.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

func (s *sliceIRStream) Close() error { return nil }

func TestAnthropicEncoderStream(t *testing.T) {
	enc := newAnthropicEncoder(mockIDGen)
	events := []ir.StreamEvent{
		{Kind: ir.EventStart, ID: "stream_1", Usage: ir.TokenUsage{Input: 10}},
		{Kind: ir.EventText, Text: "Streamed text"},
		{Kind: ir.EventStop, Stop: ir.StopEndTurn},
		{Kind: ir.EventUsage, Usage: ir.TokenUsage{Output: 8}},
		{Kind: ir.EventEnd},
	}
	st := enc.Stream(&sliceIRStream{events: events}, &protocol.Envelope{Model: "claude-3-5"})
	defer st.Close()

	var eventNames []string
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		eventNames = append(eventNames, ev.Name)
	}
	expected := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(eventNames) != len(expected) {
		t.Fatalf("expected %d events, got %d: %v", len(expected), len(eventNames), eventNames)
	}
	for i, exp := range expected {
		if eventNames[i] != exp {
			t.Errorf("event %d: expected %s, got %s", i, exp, eventNames[i])
		}
	}
}
