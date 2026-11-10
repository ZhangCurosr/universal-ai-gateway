package translate

import (
	"io"
	"os"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

func TestChatDecoderBody(t *testing.T) {
	data, err := os.ReadFile("testdata/chat_to_anthropic/01_text/input.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := chatDecoder{}
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
	if !ok || tp.Text != "Hello!" {
		t.Errorf("expected TextPart 'Hello!', got %+v", resp.Parts[0])
	}
}

func TestChatDecoderBodyRefusal(t *testing.T) {
	raw := `{
		"id": "chatcmpl-ref",
		"object": "chat.completion",
		"created": 123,
		"model": "gpt-4",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "refusal": "I cannot help"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 3}
	}`
	dec := chatDecoder{}
	resp, err := dec.Body([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(resp.Parts))
	}
	tp, ok := resp.Parts[0].(ir.TextPart)
	if !ok || !tp.IsRefusal || tp.Text != "I cannot help" {
		t.Errorf("unexpected refusal part: %+v", resp.Parts[0])
	}
}

func TestChatDecoderStream(t *testing.T) {
	events := []sse.Event{
		{Data: []byte(`{"id":"chatcmpl-1","choices":[{"delta":{"role":"assistant","content":"Hi"}}]}`)},
		{Data: []byte(`{"id":"chatcmpl-1","choices":[{"delta":{"content":" there"},"finish_reason":"stop"}]}`)},
		{Data: []byte(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)},
		{Data: []byte(`[DONE]`)},
	}
	dec := chatDecoder{}
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
