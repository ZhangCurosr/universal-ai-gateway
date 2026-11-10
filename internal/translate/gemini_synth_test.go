package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)


func TestSynthesizeGemini(t *testing.T) {
	req := &ir.Request{
		Model:  "gemini-3-pro",
		System: []ir.TextPart{{Text: "sys", CacheControl: &ir.CacheControl{Type: "ephemeral"}}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "hi"}, ir.ImagePart{MediaType: "image/png", Data: []byte("hello")}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{
				ir.ThinkingPart{Vendor: ir.VendorAnthropic, Text: "secret", Signature: "s"},
				ir.TextPart{Text: "calling"},
				ir.ToolCallPart{ID: "toolu_1", Name: "w", Arguments: json.RawMessage(`{"c":"a"}`)},
				ir.ToolCallPart{ID: "toolu_2", Name: "x"},
			}},
			{Role: ir.RoleUser, Parts: []ir.Part{
				ir.TextPart{Text: "after"},
				ir.ToolResultPart{ToolCallID: "toolu_1", Content: []ir.Part{ir.TextPart{Text: "sunny"}}},
				ir.ToolResultPart{ToolCallID: "toolu_2", IsError: true, Content: []ir.Part{ir.TextPart{Text: "boom"}}},
			}},
		},
		Tools:         []ir.Tool{{Name: "w", Description: "weather", InputSchema: json.RawMessage(`{"type":"object","const":1}`)}, {Name: "x"}},
		ToolChoice:    &ir.ToolChoice{Mode: "tool", Name: "w"},
		MaxTokens:     64,
		Temperature:   f64(0.2),
		StopSequences: []string{"END"},
		User:          "u1",
		Stream:        true,
	}
	body, err := synthesizeGemini(req)
	if err != nil {
		t.Fatalf("synthesizeGemini() error = %v", err)
	}
	want := `{"contents":[` +
		`{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]},` +
		`{"role":"model","parts":[{"text":"calling"},{"functionCall":{"name":"w","args":{"c":"a"}},"thoughtSignature":"skip_thought_signature_validator"},{"functionCall":{"name":"x","args":{}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"w","response":{"output":"sunny"}}},{"functionResponse":{"name":"x","response":{"error":"boom"}}},{"text":"after"}]}],` +
		`"systemInstruction":{"parts":[{"text":"sys"}]},` +
		`"tools":[{"functionDeclarations":[{"name":"w","description":"weather","parametersJsonSchema":{"type":"object","const":1}},{"name":"x","parametersJsonSchema":{"type":"object","properties":{}}}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["w"]}},` +
		`"generationConfig":{"maxOutputTokens":64,"temperature":0.2,"stopSequences":["END"]}}`
	if string(body) != want {
		t.Fatalf("synthesizeGemini() =\n%s\nwant\n%s", body, want)
	}
	if strings.Contains(string(body), "gemini-3-pro") || strings.Contains(string(body), "u1") {
		t.Fatalf("body must not contain model name or user: %s", body)
	}
}

func TestSynthesizeGeminiSignatureByModel(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "q"}}},
		{Role: ir.RoleAssistant, Parts: []ir.Part{ir.ToolCallPart{ID: "1", Name: "f"}}},
		{Role: ir.RoleUser, Parts: []ir.Part{ir.ToolResultPart{ToolCallID: "1", Content: []ir.Part{ir.TextPart{Text: "r"}}}}},
		{Role: ir.RoleAssistant, Parts: []ir.Part{ir.ToolCallPart{ID: "2", Name: "f"}, ir.ToolCallPart{ID: "3", Name: "f"}}},
	}
	tests := []struct {
		model string
		want  int // 期望的签名数量：每条 model 消息只有第一个 functionCall 带签名
	}{
		{"gemini-3-pro-preview", 2},
		{"models/gemini-3-flash", 2},
		{"gemini-4", 2},
		{"gemini-2.5-pro", 0},
		{"models/gemini-2.0-flash", 0},
		{"gemini-1.5-pro", 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			body, err := synthesizeGemini(&ir.Request{Model: tt.model, Messages: msgs})
			if err != nil {
				t.Fatalf("synthesizeGemini() error = %v", err)
			}
			if got := strings.Count(string(body), geminiSkipSignature); got != tt.want {
				t.Fatalf("signatures = %d, want %d: %s", got, tt.want, body)
			}
		})
	}
}

func TestSynthesizeGeminiToolChoice(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{{"auto", "AUTO"}, {"any", "ANY"}, {"none", "NONE"}} {
		body, err := synthesizeGemini(&ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "q"}}}},
			ToolChoice: &ir.ToolChoice{Mode: tt.mode}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"mode":"`+tt.want+`"`) {
			t.Errorf("mode %s -> %s", tt.mode, body)
		}
	}
}

func TestSynthesizeGeminiRejects(t *testing.T) {
	user := func(parts ...ir.Part) []ir.Message { return []ir.Message{{Role: ir.RoleUser, Parts: parts}} }
	tests := []struct {
		name string
		req  *ir.Request
		want string
	}{
		{"image url", &ir.Request{Messages: user(ir.ImagePart{URL: "https://x/y.png"})}, "image URLs are not supported"},
		{"orphan tool result", &ir.Request{Messages: user(ir.ToolResultPart{ToolCallID: "nope"})}, `tool result "nope" has no matching tool call`},
		{"image in tool result", &ir.Request{Messages: []ir.Message{
			{Role: ir.RoleAssistant, Parts: []ir.Part{ir.ToolCallPart{ID: "1", Name: "f"}}},
			{Role: ir.RoleUser, Parts: []ir.Part{ir.ToolResultPart{ToolCallID: "1", Content: []ir.Part{ir.ImagePart{Data: []byte("x")}}}}},
		}}, "images in tool results"},
		{"too many stops", &ir.Request{Messages: user(ir.TextPart{Text: "q"}), StopSequences: []string{"1", "2", "3", "4", "5", "6"}}, "at most 5"},
		{"empty", &ir.Request{Messages: user(ir.TextPart{Text: ""})}, "messages must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := synthesizeGemini(tt.req)
			wantInvalid(t, err, tt.want)
		})
	}
}
