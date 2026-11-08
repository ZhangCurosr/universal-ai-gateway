package translate

import (
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// spec/10 §7 + §8.4：responses → IR 方向的黄金文件用例（testdata/responses_to_ir/<用例>/{input,expected}.json）。
func TestLowerOpenAIResponsesGolden(t *testing.T) {
	runLowerGolden(t, "responses_to_ir", lowerOpenAIResponses)
}

// spec/10 §8.1、§8.2、§8.4：有状态特性、内置工具与无法表达的特性一律 400 invalid_request。
func TestLowerOpenAIResponsesRejects(t *testing.T) {
	tests := []struct {
		name, body, wantInMsg string
	}{
		{"not object", `"hi"`, "JSON object"},
		{"missing model", `{"input":"hi"}`, "model"},
		{"missing input", `{"model":"m"}`, "input"},
		{"empty input", `{"model":"m","input":[]}`, "input"},
		// RX-01（含规定的消息格式）
		{"previous_response_id", `{"model":"claude-x","input":"hi","previous_response_id":"resp_1"}`,
			`previous_response_id is not supported for model "claude-x"; send the full input instead`},
		{"conversation", `{"model":"m","input":"hi","conversation":"conv_1"}`, "conversation"},
		// RX-03
		{"background", `{"model":"m","input":"hi","background":true}`, "background"},
		// 内置工具（规定的消息格式）
		{"web_search tool", `{"model":"claude-x","input":"hi","tools":[{"type":"web_search"}]}`,
			`built-in tool "web_search" is not supported for model "claude-x"`},
		{"mcp tool", `{"model":"m","input":"hi","tools":[{"type":"mcp","server_label":"x"}]}`, `"mcp"`},
		{"json_schema format", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","schema":{}}}}`, "json_schema"},
		{"json_object format", `{"model":"m","input":"hi","text":{"format":{"type":"json_object"}}}`, "json_object"},
		{"unknown top-level", `{"model":"m","input":"hi","prompt":{"id":"p"}}`, "prompt"},
		{"input_file", `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]}`, "input_file"},
		{"image by file_id", `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","file_id":"f"}]}]}`, "file_id"},
		{"image in assistant", `{"model":"m","input":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`, "user messages"},
		{"system after user", `{"model":"m","input":[{"role":"user","content":"hi"},{"role":"system","content":"s"}]}`, "before other"},
		{"only system", `{"model":"m","input":[{"role":"system","content":"s"}]}`, "at least one"},
		{"bad role", `{"model":"m","input":[{"role":"tool","content":"x"}]}`, "role"},
		{"unknown item type", `{"model":"m","input":[{"type":"web_search_call","id":"ws_1"}]}`, "web_search_call"},
		{"function_call without call_id", `{"model":"m","input":[{"role":"user","content":"hi"},{"type":"function_call","name":"f","arguments":"{}"}]}`, "call_id"},
		{"bad arguments", `{"model":"m","input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"c","name":"f","arguments":"[1]"}]}`, "arguments"},
		{"output without call_id", `{"model":"m","input":[{"type":"function_call_output","output":"x"}]}`, "call_id"},
		{"bad tool_choice", `{"model":"m","input":"hi","tool_choice":{"type":"web_search"}}`, "tool_choice"},
		{"zero max_output_tokens", `{"model":"m","input":"hi","max_output_tokens":0}`, "max_output_tokens"},
		{"bad data url", `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:text/plain,x"}]}]}`, "data URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lowerOpenAIResponses([]byte(tt.body))
			ae, ok := apperr.As(err)
			if !ok || ae.Code != apperr.CodeInvalidRequest {
				t.Fatalf("error = %v, want invalid_request", err)
			}
			if !strings.Contains(ae.Message, tt.wantInMsg) {
				t.Errorf("message %q should mention %q", ae.Message, tt.wantInMsg)
			}
		})
	}
}

// null 视为未提供；background=false、store=true（RX-02 视为 false）放行；空 arguments 视为 {}。
func TestLowerOpenAIResponsesAcceptsNeutral(t *testing.T) {
	body := `{"model":"m","background":false,"store":true,"previous_response_id":null,"prompt":null,
		"input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"c","name":"f","arguments":""}]}`
	req, err := lowerOpenAIResponses([]byte(body))
	if err != nil {
		t.Fatalf("lowerOpenAIResponses() error = %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
}
