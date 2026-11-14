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

func TestResponsesEncoderBody(t *testing.T) {
	now := time.Unix(1700000000, 0)
	enc := newResponsesEncoder(now, mockIDGen)
	resp := &ir.Response{
		ID:         "upstream_resp",
		Model:      "model",
		StopReason: ir.StopEndTurn,
		Parts: []ir.Part{
			ir.TextPart{Text: "Responses text output"},
		},
		Usage: ir.TokenUsage{Input: 15, Output: 10},
	}
	body, err := enc.Body(resp, &protocol.Envelope{Model: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	var out respObject
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("expected status completed, got %s", out.Status)
	}
	if len(out.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(out.Output))
	}
	item := out.Output[0]
	if item.Type != "message" || len(item.Content) != 1 || item.Content[0].Text != "Responses text output" {
		t.Errorf("unexpected output item: %+v", item)
	}
}

func TestResponsesEncoderStream(t *testing.T) {
	now := time.Unix(1700000000, 0)
	enc := newResponsesEncoder(now, mockIDGen)
	events := []ir.StreamEvent{
		{Kind: ir.EventStart, ID: "stream_resp"},
		{Kind: ir.EventText, Text: "Streamed responses"},
		{Kind: ir.EventStop, Stop: ir.StopEndTurn},
		{Kind: ir.EventUsage, Usage: ir.TokenUsage{Input: 10, Output: 5}},
		{Kind: ir.EventEnd},
	}
	st := enc.Stream(&sliceIRStream{events: events}, &protocol.Envelope{Model: "gpt-4o"})
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
	// response.created, response.in_progress, output_item.added, content_part.added, delta, done, done, done, response.completed
	if len(eventNames) < 5 {
		t.Fatalf("expected multiple responses events, got %d: %v", len(eventNames), eventNames)
	}
	if eventNames[0] != "response.created" {
		t.Errorf("expected first event response.created, got %s", eventNames[0])
	}
	if eventNames[len(eventNames)-1] != "response.completed" {
		t.Errorf("expected last event response.completed, got %s", eventNames[len(eventNames)-1])
	}
}

// TC-02（T75）：message 项总是带 content（Codex 要求）；function_call 项不带。
func TestRespOutputItemContent(t *testing.T) {
	msg, err := json.Marshal(respOutputItem{Type: "message", ID: "m", Status: "in_progress", Role: "assistant"})
	if err != nil || !strings.Contains(string(msg), `"content":[]`) || strings.Count(string(msg), `"content"`) != 1 {
		t.Fatalf("message = %s %v", msg, err)
	}
	args := ""
	call, err := json.Marshal(respOutputItem{Type: "function_call", ID: "f", Status: "in_progress", CallID: "c", Name: "n", Arguments: &args})
	if err != nil || strings.Contains(string(call), "content") || !strings.Contains(string(call), `"arguments":""`) {
		t.Fatalf("function_call = %s %v", call, err)
	}
}
