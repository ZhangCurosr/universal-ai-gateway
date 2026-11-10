package translate

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

func TestAnthropicDecoderBody(t *testing.T) {
	data, err := os.ReadFile("testdata/anthropic_to_chat/01_text/input.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := anthropicDecoder{}
	resp, err := dec.Body(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "up1" {
		t.Errorf("expected ID up1, got %q", resp.ID)
	}
	if resp.StopReason != ir.StopEndTurn {
		t.Errorf("expected StopEndTurn, got %v", resp.StopReason)
	}
	if len(resp.Parts) == 0 {
		t.Fatal("expected parts")
	}
	tp, ok := resp.Parts[0].(ir.TextPart)
	if !ok || tp.Text != "Hello" {
		t.Errorf("expected TextPart 'Hello', got %+v", resp.Parts[0])
	}
}

func TestAnthropicDecoderBodyToolUse(t *testing.T) {
	raw := `{
		"id": "msg_tool",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5",
		"content": [
			{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "Paris"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	dec := anthropicDecoder{}
	resp, err := dec.Body([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("expected StopToolUse, got %v", resp.StopReason)
	}
	if len(resp.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(resp.Parts))
	}
	tc, ok := resp.Parts[0].(ir.ToolCallPart)
	if !ok || tc.Name != "get_weather" || tc.ID != "call_1" {
		t.Errorf("unexpected tool part: %+v", resp.Parts[0])
	}
	var m map[string]string
	if err := json.Unmarshal(tc.Arguments, &m); err != nil || m["city"] != "Paris" {
		t.Errorf("unexpected arguments: %s", string(tc.Arguments))
	}
}

func TestAnthropicDecoderStream(t *testing.T) {
	events := []sse.Event{
		{Name: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_123","model":"claude","usage":{"input_tokens":10,"output_tokens":0}}}`)},
		{Name: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)},
		{Name: "content_block_delta", Data: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)},
		{Name: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
		{Name: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)},
		{Name: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	}
	dec := anthropicDecoder{}
	st := dec.Stream(&sliceStream{evs: events})
	defer st.Close()

	var gotKinds []ir.StreamEventKind
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		gotKinds = append(gotKinds, ev.Kind)
	}
	expected := []ir.StreamEventKind{
		ir.EventStart,
		ir.EventText,
		ir.EventStop,
		ir.EventUsage,
		ir.EventEnd,
	}
	if len(gotKinds) != len(expected) {
		t.Fatalf("expected %d events, got %d: %+v", len(expected), len(gotKinds), gotKinds)
	}
	for i, exp := range expected {
		if gotKinds[i] != exp {
			t.Errorf("event %d: expected %v, got %v", i, exp, gotKinds[i])
		}
	}
}
