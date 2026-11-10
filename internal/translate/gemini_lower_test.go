package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func geminiEnv(body string) *protocol.Envelope {
	return &protocol.Envelope{
		Protocol: protoGemini, Op: opGeminiCall, Model: "gemini-client-model",
		Body: []byte(body), Params: map[string]string{"method": "generateContent"},
	}
}

func mustLowerGemini(t *testing.T, body string) *ir.Request {
	t.Helper()
	req, err := lowerGemini(geminiEnv(body), Options{}, "gemini", "anthropic")
	if err != nil {
		t.Fatalf("lowerGemini(%s) error = %v", body, err)
	}
	return req
}

func TestLowerGeminiBasic(t *testing.T) {
	req := mustLowerGemini(t, `{
		"systemInstruction": {"parts": [{"text": "be brief"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "hi"}, {"inlineData": {"mimeType": "image/png", "data": "aGVsbG8="}}]},
			{"role": "model", "parts": [{"text": "think", "thought": true, "thoughtSignature": "sig"}, {"text": "hello", "thoughtSignature": "dropme"}]},
			{"parts": [{"text": "again"}]}
		],
		"generationConfig": {"maxOutputTokens": 100, "temperature": 0.5, "topP": 0.9, "stopSequences": ["x"], "topK": 40,
			"thinkingConfig": {"thinkingBudget": 10}, "candidateCount": 1, "responseMimeType": "text/plain"},
		"safetySettings": [{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"}]
	}`)
	if req.Model != "gemini-client-model" || len(req.System) != 1 || req.System[0].Text != "be brief" {
		t.Fatalf("model/system = %q %+v", req.Model, req.System)
	}
	if len(req.Messages) != 3 || req.Messages[1].Role != ir.RoleAssistant || req.Messages[2].Role != ir.RoleUser {
		t.Fatalf("messages = %+v", req.Messages)
	}
	img, ok := req.Messages[0].Parts[1].(ir.ImagePart)
	if !ok || img.MediaType != "image/png" || string(img.Data) != "hello" {
		t.Fatalf("image part = %+v", req.Messages[0].Parts[1])
	}
	th, ok := req.Messages[1].Parts[0].(ir.ThinkingPart)
	if !ok || th.Vendor != ir.VendorGemini || th.Signature != "sig" || th.Text != "think" {
		t.Fatalf("thinking part = %+v", req.Messages[1].Parts[0])
	}
	if tp, ok := req.Messages[1].Parts[1].(ir.TextPart); !ok || tp.Text != "hello" {
		t.Fatalf("text part = %+v", req.Messages[1].Parts[1])
	}
	if req.MaxTokens != 100 || *req.Temperature != 0.5 || *req.TopP != 0.9 || len(req.StopSequences) != 1 {
		t.Fatalf("generation config = %+v", req)
	}
}

func TestLowerGeminiToolIDs(t *testing.T) {
	t.Run("GID-03/GID-04 parallel same-name calls without id", func(t *testing.T) {
		req := mustLowerGemini(t, `{"contents": [
			{"role": "user", "parts": [{"text": "weather?"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "w", "args": {"c": "a"}}}, {"functionCall": {"name": "w", "args": {"c": "b"}}}]},
			{"role": "user", "parts": [{"text": "here"}, {"functionResponse": {"name": "w", "response": {"t": 1}}}, {"functionResponse": {"name": "w", "response": {"t": 2}}}]}
		]}`)
		c0 := req.Messages[1].Parts[0].(ir.ToolCallPart)
		c1 := req.Messages[1].Parts[1].(ir.ToolCallPart)
		if c0.ID != "gemini_call_0" || c1.ID != "gemini_call_1" || string(c0.Arguments) != `{"c": "a"}` {
			t.Fatalf("calls = %+v %+v", c0, c1)
		}
		user := req.Messages[2].Parts
		r0, r1 := user[0].(ir.ToolResultPart), user[1].(ir.ToolResultPart)
		if r0.ToolCallID != "gemini_call_0" || r1.ToolCallID != "gemini_call_1" {
			t.Fatalf("results = %+v %+v (want ordered match)", r0, r1)
		}
		if txt := r0.Content[0].(ir.TextPart).Text; txt != `{"t":1}` {
			t.Fatalf("result content = %q, want compact JSON", txt)
		}
		if _, ok := user[2].(ir.TextPart); !ok {
			t.Fatalf("tool results must come before other parts, got %+v", user)
		}
	})
	t.Run("GID-04 by id", func(t *testing.T) {
		req := mustLowerGemini(t, `{"contents": [
			{"role": "model", "parts": [{"functionCall": {"id": "a1", "name": "f"}}, {"functionCall": {"id": "a2", "name": "f"}}]},
			{"role": "function", "parts": [{"functionResponse": {"id": "a2", "name": "f", "response": {}}}]}
		]}`)
		if got := req.Messages[1].Parts[0].(ir.ToolResultPart).ToolCallID; got != "a2" {
			t.Fatalf("ToolCallID = %q, want a2", got)
		}
		if args := req.Messages[0].Parts[0].(ir.ToolCallPart).Arguments; string(args) != `{}` {
			t.Fatalf("missing args = %s, want {}", args)
		}
	})
	t.Run("GID-04 no match", func(t *testing.T) {
		_, err := lowerGemini(geminiEnv(`{"contents": [
			{"role": "model", "parts": [{"functionCall": {"name": "f"}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "g", "response": {}}}]}
		]}`), Options{}, "gemini", "anthropic")
		wantInvalid(t, err, "has no matching functionCall")
	})
}

func TestLowerGeminiTools(t *testing.T) {
	req := mustLowerGemini(t, `{"contents": [{"parts": [{"text": "x"}]}],
		"tools": [{"functionDeclarations": [
			{"name": "a", "description": "d", "parametersJsonSchema": {"type": "object", "const": 1}},
			{"name": "b", "parameters": {"type": "OBJECT", "propertyOrdering": ["p"], "properties": {"p": {"type": "STRING", "nullable": true, "maxLength": 10}}}},
			{"name": "c"}
		]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["b"]}}}`)
	if len(req.Tools) != 3 {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if string(req.Tools[0].InputSchema) != `{"type": "object", "const": 1}` {
		t.Errorf("parametersJsonSchema not passed through: %s", req.Tools[0].InputSchema)
	}
	var got any
	if err := json.Unmarshal(req.Tools[1].InputSchema, &got); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if want := `{"properties":{"p":{"maxLength":10,"type":["string","null"]}},"type":"object"}`; string(b) != want {
		t.Errorf("GL-03 schema = %s, want %s", b, want)
	}
	if string(req.Tools[2].InputSchema) != `{"type":"object","properties":{}}` {
		t.Errorf("default schema = %s", req.Tools[2].InputSchema)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != "tool" || req.ToolChoice.Name != "b" {
		t.Errorf("tool choice = %+v", req.ToolChoice)
	}

	for _, tt := range []struct{ mode, want string }{{"AUTO", "auto"}, {"NONE", "none"}, {"ANY", "any"}} {
		req := mustLowerGemini(t, `{"contents": [{"parts": [{"text": "x"}]}], "toolConfig": {"functionCallingConfig": {"mode": "`+tt.mode+`"}}}`)
		if req.ToolChoice.Mode != tt.want {
			t.Errorf("mode %s -> %q, want %q", tt.mode, req.ToolChoice.Mode, tt.want)
		}
	}
}

func TestLowerGeminiRejects(t *testing.T) {
	base := func(extra string) string { return `{"contents": [{"parts": [{"text": "x"}]}]` + extra + `}` }
	tests := []struct {
		name, body, want string
	}{
		{"built-in tool", base(`,"tools":[{"googleSearch":{}}]`), `built-in tool "googleSearch"`},
		{"fileData", `{"contents":[{"parts":[{"fileData":{"fileUri":"u"}}]}]}`, "part type fileData"},
		{"executableCode", `{"contents":[{"parts":[{"executableCode":{"code":"1"}}]}]}`, "part type executableCode"},
		{"non-image inlineData", `{"contents":[{"parts":[{"inlineData":{"mimeType":"application/pdf","data":"AA=="}}]}]}`, `inlineData mimeType "application/pdf"`},
		{"bad base64", `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"@@"}}]}]}`, "not valid base64"},
		{"candidateCount", base(`,"generationConfig":{"candidateCount":2}`), "candidateCount > 1"},
		{"responseSchema", base(`,"generationConfig":{"responseSchema":{}}`), "generationConfig.responseSchema"},
		{"responseJsonSchema", base(`,"generationConfig":{"responseJsonSchema":{}}`), "generationConfig.responseJsonSchema"},
		{"json mime", base(`,"generationConfig":{"responseMimeType":"application/json"}`), "responseMimeType"},
		{"image modality", base(`,"generationConfig":{"responseModalities":["IMAGE"]}`), "responseModalities"},
		{"seed", base(`,"generationConfig":{"seed":1}`), "generationConfig.seed"},
		{"cachedContent", base(`,"cachedContent":"cachedContents/1"`), "cachedContent"},
		{"unknown top", base(`,"foo":1`), "foo is not supported"},
		{"unknown genconfig", base(`,"generationConfig":{"bar":1}`), "generationConfig.bar is not supported"},
		{"empty contents", `{"contents":[]}`, "contents must not be empty"},
		{"bad role", `{"contents":[{"role":"system","parts":[{"text":"x"}]}]}`, `role "system"`},
		{"args not object", `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":[1]}}]}]}`, "args must be an object"},
		{"VALIDATED mode", base(`,"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}}`), `mode "VALIDATED"`},
		{"two allowed names", base(`,"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["a","b"]}}`), "more than one name"},
		{"response parts", `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f"}}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{},"parts":[{"inlineData":{}}]}}]}]}`, "functionResponse.parts"},
		{"system non-text", base(`,"systemInstruction":{"parts":[{"inlineData":{}}]}`), "only text parts"},
		{"maxOutputTokens zero", base(`,"generationConfig":{"maxOutputTokens":0}`), "positive integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lowerGemini(geminiEnv(tt.body), Options{}, "gemini", "anthropic")
			wantInvalid(t, err, tt.want)
		})
	}
}

func TestLowerGeminiDropUnknown(t *testing.T) {
	req, err := lowerGemini(geminiEnv(`{"contents":[{"parts":[{"text":"x"}]}],"foo":1,"generationConfig":{"bar":2}}`),
		Options{DropUnknownFields: true}, "gemini", "anthropic")
	if err != nil || len(req.Messages) != 1 {
		t.Fatalf("drop mode: req=%+v err=%v", req, err)
	}
}

func TestAcceptGemini(t *testing.T) {
	env := geminiEnv(`{}`)
	if err := acceptGemini(env); err != nil {
		t.Fatalf("generateContent rejected: %v", err)
	}
	env.Params["method"] = "streamGenerateContent"
	if err := acceptGemini(env); err != nil {
		t.Fatalf("streamGenerateContent rejected: %v", err)
	}
	env.Params["method"] = "countTokens"
	wantInvalid(t, acceptGemini(env), `countTokens is not supported for model "gemini-client-model"`)
}

func wantInvalid(t *testing.T, err error, contains string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeInvalidRequest {
		t.Fatalf("error = %v, want invalid_request", err)
	}
	if !strings.Contains(ae.Message, contains) {
		t.Fatalf("error message = %q, want it to contain %q", ae.Message, contains)
	}
}
