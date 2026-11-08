package translate

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

// -update 重新生成黄金文件：go test ./internal/translate -run Golden -update（生成后必须人工核对）。
var update = flag.Bool("update", false, "rewrite golden files")

// dumpPart 把 IR 内容块转换为带类型标签的可比较结构（Part 是接口，直接 Marshal 会丢失类型）。
func dumpPart(p ir.Part) map[string]any {
	switch v := p.(type) {
	case ir.TextPart:
		m := map[string]any{"type": "text", "text": v.Text}
		if v.CacheControl != nil {
			m["cache_control"] = v.CacheControl
		}
		return m
	case ir.ImagePart:
		m := map[string]any{"type": "image", "media_type": v.MediaType, "url": v.URL}
		if v.Data != nil {
			m["data"] = v.Data // []byte 以 base64 输出
		}
		return m
	case ir.ToolCallPart:
		return map[string]any{"type": "tool_call", "id": v.ID, "name": v.Name, "arguments": v.Arguments}
	case ir.ToolResultPart:
		content := make([]any, len(v.Content))
		for i, c := range v.Content {
			content[i] = dumpPart(c)
		}
		return map[string]any{"type": "tool_result", "tool_call_id": v.ToolCallID, "is_error": v.IsError, "content": content}
	case ir.ThinkingPart:
		return map[string]any{"type": "thinking", "vendor": v.Vendor, "text": v.Text, "signature": v.Signature,
			"redacted": v.Redacted, "redacted_data": v.RedactedData, "encrypted": v.Encrypted}
	default:
		return map[string]any{"type": "unknown"}
	}
}

func dumpRequest(r *ir.Request) []byte {
	msgs := make([]any, len(r.Messages))
	for i, m := range r.Messages {
		parts := make([]any, len(m.Parts))
		for j, p := range m.Parts {
			parts[j] = dumpPart(p)
		}
		msgs[i] = map[string]any{"role": m.Role, "parts": parts}
	}
	system := make([]any, len(r.System))
	for i, s := range r.System {
		system[i] = dumpPart(s)
	}
	out := map[string]any{
		"model": r.Model, "system": system, "messages": msgs,
		"tools": r.Tools, "tool_choice": r.ToolChoice, "max_tokens": r.MaxTokens,
		"temperature": r.Temperature, "top_p": r.TopP, "stop_sequences": r.StopSequences,
		"stream": r.Stream, "user": r.User,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err) // 测试辅助：结构全部可序列化
	}
	return append(b, '\n')
}

// spec/10 §7：openai → IR 方向的黄金文件用例（testdata/chat_to_ir/<用例>/{input,expected}.json）。
func TestLowerOpenAIChatGolden(t *testing.T) {
	runLowerGolden(t, "chat_to_ir", lowerOpenAIChat)
}

// runLowerGolden 对 testdata/<group>/* 中每个用例运行 lower，并与 expected.json 比较。
func runLowerGolden(t *testing.T, group string, lower func([]byte) (*ir.Request, error)) {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("testdata", group, "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no golden cases found in %s: %v", group, err)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join(dir, "input.json"))
			if err != nil {
				t.Fatal(err)
			}
			req, err := lower(input)
			if err != nil {
				t.Fatalf("lower() error = %v", err)
			}
			got := dumpRequest(req)
			expPath := filepath.Join(dir, "expected.json")
			if *update {
				if err := os.WriteFile(expPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(expPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
				t.Errorf("IR mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", dir, got, want)
			}
		})
	}
}

// spec/10 §3 + §7 第 9 条：无法表达的特性一律 400 invalid_request，消息指出字段。
func TestLowerOpenAIChatRejects(t *testing.T) {
	msgs := `"messages":[{"role":"user","content":"hi"}]`
	tests := []struct {
		name, body, wantInMsg string
	}{
		{"not object", `[1]`, "JSON object"},
		{"missing model", `{` + msgs + `}`, "model"},
		{"missing messages", `{"model":"m"}`, "messages"},
		{"empty messages", `{"model":"m","messages":[]}`, "messages"},
		{"n>1", `{"model":"m","n":2,` + msgs + `}`, "n > 1"},
		{"response_format", `{"model":"m","response_format":{"type":"json_object"},` + msgs + `}`, "response_format"},
		{"logprobs", `{"model":"m","logprobs":true,` + msgs + `}`, "logprobs"},
		{"audio", `{"model":"m","audio":{"voice":"x"},` + msgs + `}`, "audio"},
		{"modalities", `{"model":"m","modalities":["text","audio"],` + msgs + `}`, "modalities"},
		{"unknown top-level field", `{"model":"m","seed":1,` + msgs + `}`, "seed"},
		{"parallel_tool_calls false", `{"model":"m","parallel_tool_calls":false,` + msgs + `}`, "parallel_tool_calls"},
		{"system after user", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":"s"}]}`, "before user"},
		{"only system", `{"model":"m","messages":[{"role":"system","content":"s"}]}`, "at least one"},
		{"bad role", `{"model":"m","messages":[{"role":"function","content":"x"}]}`, "role"},
		{"bad tool arguments", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{not json"}}]}]}`, "arguments"},
		{"array tool arguments", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"[1]"}}]}]}`, "arguments"},
		{"empty assistant", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":""}]}`, "content or tool_calls"},
		{"tool without id", `{"model":"m","messages":[{"role":"tool","content":"r"}]}`, "tool_call_id"},
		{"non-function tool", `{"model":"m","tools":[{"type":"web_search"}],` + msgs + `}`, "only function"},
		{"strict tool", `{"model":"m","tools":[{"type":"function","function":{"name":"f","strict":true}}],` + msgs + `}`, "strict"},
		{"bad tool_choice", `{"model":"m","tool_choice":"sometimes",` + msgs + `}`, "tool_choice"},
		{"zero max_tokens", `{"model":"m","max_tokens":0,` + msgs + `}`, "max_tokens"},
		{"input_audio part", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`, "input_audio"},
		{"image in system", `{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]},{"role":"user","content":"hi"}]}`, "image_url"},
		{"bad data url", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:text/plain,hello"}}]}]}`, "data URL"},
		{"bad base64", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,!!!"}}]}]}`, "base64"},
		{"ftp image", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"ftp://x/y.png"}}]}]}`, "http(s)"},
		{"refusal in history", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x","refusal":"no"}]}`, "refusal"},
		{"unknown message field", `{"model":"m","messages":[{"role":"user","content":"hi","audio":{"id":"a"}}]}`, "audio"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lowerOpenAIChat([]byte(tt.body))
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

// 明确放行的写法：null 视为未提供；n=1、logprobs=false、parallel_tool_calls=true、stream_options。
func TestLowerOpenAIChatAcceptsNeutralFields(t *testing.T) {
	body := `{"model":"m","n":1,"logprobs":false,"parallel_tool_calls":true,"stream":true,
		"stream_options":{"include_usage":true},"response_format":null,"seed":null,
		"messages":[{"role":"user","content":"hi","name":"alice"}]}`
	req, err := lowerOpenAIChat([]byte(body))
	if err != nil {
		t.Fatalf("lowerOpenAIChat() error = %v", err)
	}
	if !req.Stream || len(req.Messages) != 1 {
		t.Errorf("req = %+v", req)
	}
}

// max_completion_tokens 优先于 max_tokens。
func TestLowerOpenAIChatMaxCompletionTokensWins(t *testing.T) {
	req, err := lowerOpenAIChat([]byte(`{"model":"m","max_tokens":10,"max_completion_tokens":20,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 20 {
		t.Errorf("MaxTokens = %d, want 20", req.MaxTokens)
	}
}
