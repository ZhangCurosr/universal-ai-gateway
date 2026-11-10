package translate

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestChatEncoderBody(t *testing.T) {
	now := time.Unix(1700000000, 0)
	enc := newChatEncoder(now, mockIDGen)
	resp := &ir.Response{
		ID:         "msg_123",
		Model:      "model",
		StopReason: ir.StopEndTurn,
		Parts: []ir.Part{
			ir.TextPart{Text: "Chat reply"},
			ir.ThinkingPart{Text: "Thinking steps"},
		},
		Usage: ir.TokenUsage{Input: 20, Output: 10, CacheRead: 5},
	}
	body, err := enc.Body(resp, &protocol.Envelope{Model: "gpt-4"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "chatcmpl-123" {
		t.Errorf("expected chatcmpl-123, got %v", out["id"])
	}
	if out["model"] != "gpt-4" {
		t.Errorf("expected gpt-4, got %v", out["model"])
	}
	choices := out["choices"].([]any)
	c := choices[0].(map[string]any)
	msg := c["message"].(map[string]any)
	if msg["content"] != "Chat reply" {
		t.Errorf("expected content 'Chat reply', got %v", msg["content"])
	}
	if msg["reasoning_content"] != "Thinking steps" {
		t.Errorf("expected reasoning_content 'Thinking steps', got %v", msg["reasoning_content"])
	}
}

func TestChatEncoderStream(t *testing.T) {
	now := time.Unix(1700000000, 0)
	enc := newChatEncoder(now, mockIDGen)
	events := []ir.StreamEvent{
		{Kind: ir.EventStart, ID: "stream_chat"},
		{Kind: ir.EventText, Text: "Stream content"},
		{Kind: ir.EventStop, Stop: ir.StopEndTurn},
		{Kind: ir.EventUsage, Usage: ir.TokenUsage{Input: 10, Output: 5}},
		{Kind: ir.EventEnd},
	}
	st := enc.Stream(&sliceIRStream{events: events}, &protocol.Envelope{
		Model: "gpt-4",
		Body:  []byte(`{"stream_options":{"include_usage":true}}`),
	})
	defer st.Close()

	var chunks []string
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, string(ev.Data))
	}
	// start (role), text delta, finish reason, usage chunk, [DONE]
	if len(chunks) != 5 {
		t.Fatalf("expected 5 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[4] != "[DONE]" {
		t.Errorf("expected [DONE], got %s", chunks[4])
	}
	if !strings.Contains(chunks[3], `"usage"`) {
		t.Errorf("expected usage chunk, got %s", chunks[3])
	}
}
