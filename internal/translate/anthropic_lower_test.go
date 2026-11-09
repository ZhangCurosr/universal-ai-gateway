package translate

import (
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// spec/10 §6.4 + §7：anthropic → IR 方向的黄金文件用例（testdata/anthropic_to_ir/<用例>/{input,expected}.json）。
func TestLowerAnthropicGolden(t *testing.T) {
	runLowerGolden(t, "anthropic_to_ir", lowerAnthropic)
}

// spec/10 §6.1：无法表达的特性一律 400 invalid_request，消息指出字段。
func TestLowerAnthropicRejects(t *testing.T) {
	msgs := `"messages":[{"role":"user","content":"hi"}]`
	base := `"model":"m","max_tokens":10,`
	tests := []struct {
		name, body, wantInMsg string
	}{
		{"not object", `[1]`, "JSON object"},
		{"missing model", `{"max_tokens":10,` + msgs + `}`, "model"},
		{"missing max_tokens", `{"model":"m",` + msgs + `}`, "max_tokens"},
		{"zero max_tokens", `{"model":"m","max_tokens":0,` + msgs + `}`, "max_tokens"},
		{"missing messages", `{` + base + `"stream":false}`, "messages"},
		{"empty messages", `{` + base + `"messages":[]}`, "messages"},
		{"top_k", `{` + base + `"top_k":5,` + msgs + `}`, "top_k"},
		{"mcp_servers", `{` + base + `"mcp_servers":[{}],` + msgs + `}`, "mcp_servers"},
		{"container", `{` + base + `"container":"c1",` + msgs + `}`, "container"},
		{"unknown field", `{` + base + `"foo":1,` + msgs + `}`, "foo"},
		{"server tool", `{` + base + `"tools":[{"type":"web_search_20250305","name":"web_search"}],` + msgs + `}`, `built-in tool "web_search_20250305"`},
		{"tool without name", `{` + base + `"tools":[{"input_schema":{}}],` + msgs + `}`, "name"},
		{"disable parallel", `{` + base + `"tool_choice":{"type":"auto","disable_parallel_tool_use":true},` + msgs + `}`, "disable_parallel_tool_use"},
		{"bad tool_choice", `{` + base + `"tool_choice":{"type":"sometimes"},` + msgs + `}`, "tool_choice"},
		{"tool_choice tool no name", `{` + base + `"tool_choice":{"type":"tool"},` + msgs + `}`, "name"},
		{"system image", `{` + base + `"system":[{"type":"image"}],` + msgs + `}`, "system[0]"},
		{"bad role", `{` + base + `"messages":[{"role":"system","content":"x"}]}`, "role"},
		{"message extra key", `{` + base + `"messages":[{"role":"user","content":"x","name":"a"}]}`, "name"},
		{"document block", `{` + base + `"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`, "document"},
		{"server_tool_use block", `{` + base + `"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s"}]}]}`, "server_tool_use"},
		{"tool_use in user", `{` + base + `"messages":[{"role":"user","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]}]}`, "assistant"},
		{"tool_result in assistant", `{` + base + `"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"t"}]}]}`, "user"},
		{"thinking in user", `{` + base + `"messages":[{"role":"user","content":[{"type":"thinking","thinking":"x"}]}]}`, "assistant"},
		{"tool_use array input", `{` + base + `"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":[1]}]}]}`, "input"},
		{"tool_use no id", `{` + base + `"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"f","input":{}}]}]}`, "id"},
		{"tool_result no id", `{` + base + `"messages":[{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`, "tool_use_id"},
		{"tool_result document", `{` + base + `"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"document"}]}]}]}`, "document"},
		{"image file source", `{` + base + `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"f"}}]}]}`, "file"},
		{"image bad base64", `{` + base + `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"!!!"}}]}]}`, "base64"},
		{"image not image", `{` + base + `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"text/plain","data":"aGk="}}]}]}`, "media_type"},
		{"image ftp", `{` + base + `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"ftp://x/y.png"}}]}]}`, "http(s)"},
		{"image no source", `{` + base + `"messages":[{"role":"user","content":[{"type":"image"}]}]}`, "source"},
		{"empty content", `{` + base + `"messages":[{"role":"user","content":[]}]}`, "empty"},
		{"bad stop_sequences", `{` + base + `"stop_sequences":"END",` + msgs + `}`, "stop_sequences"},
		{"bad temperature", `{` + base + `"temperature":"hot",` + msgs + `}`, "temperature"},
		{"bad metadata", `{` + base + `"metadata":[1],` + msgs + `}`, "metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lowerAnthropic([]byte(tt.body))
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

// §6.1 丢弃的字段：thinking、service_tier、context_management，以及 null 值。
func TestLowerAnthropicDropsFields(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"thinking":{"type":"adaptive"},"service_tier":"auto",
		"context_management":{"edits":[]},"top_k":null,"metadata":{"user_id":"u","other":1},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","citations":[]}]}]}`
	req, err := lowerAnthropic([]byte(body))
	if err != nil {
		t.Fatalf("lowerAnthropic() error = %v", err)
	}
	if req.User != "u" || len(req.Messages) != 1 {
		t.Errorf("req = %+v", req)
	}
}
