package translate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

// spec/10 §6.4 + §7：IR → openai_chat 方向的用例。IR 直接构造，不经过 §6.1 的解析（§1 第 4 条）。
var chatSynthCases = map[string]*ir.Request{
	"01_text": {Model: "deepseek-chat", Messages: []ir.Message{user(text("Hello"))}},

	"02_multi_turn": {Model: "deepseek-chat", Messages: []ir.Message{
		user(text("Hi")), assistant(text("Hello!")), user(text("Tell me a joke")),
	}},

	// 每个 System TextPart 一条 system 消息；cache_control 丢弃
	"03_system": {Model: "deepseek-chat",
		System:   []ir.TextPart{{Text: "You are terse."}, {Text: "Long context...", CacheControl: &ir.CacheControl{Type: "ephemeral"}}},
		Messages: []ir.Message{user(text("Hi"))},
	},

	// is_error 丢弃；只有工具调用的 assistant 消息 content 为 null
	"04_tool_result": {Model: "deepseek-chat",
		Tools:      []ir.Tool{weatherTool},
		ToolChoice: &ir.ToolChoice{Mode: "auto"},
		Messages: []ir.Message{
			user(text("Weather in Paris?")),
			assistant(ir.ToolCallPart{ID: "toolu_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}),
			user(ir.ToolResultPart{ToolCallID: "toolu_1", Content: []ir.Part{text("time"), text("out")}, IsError: true}),
		},
	},

	// 并行两个工具调用；工具结果拆为两条 tool 消息，放在同一条 user 消息的其余内容之前
	"05_parallel_tools": {Model: "deepseek-chat",
		Tools:      []ir.Tool{weatherTool},
		ToolChoice: &ir.ToolChoice{Mode: "tool", Name: "get_weather"},
		Messages: []ir.Message{
			user(text("Paris and Rome?")),
			assistant(text("Checking both."),
				ir.ToolCallPart{ID: "toolu_a", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)},
				ir.ToolCallPart{ID: "toolu_b", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Rome"}`)}),
			user(ir.ToolResultPart{ToolCallID: "toolu_a", Content: []ir.Part{text("18C")}},
				ir.ToolResultPart{ToolCallID: "toolu_b", Content: []ir.Part{text("21C")}},
				text("Thanks")),
		},
	},

	// 多块 / 含图片的 user 内容用数组；Data 编码为 data URL
	"06_images": {Model: "deepseek-chat", Messages: []ir.Message{user(
		text("Compare"),
		ir.ImagePart{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
		ir.ImagePart{URL: "https://example.com/cat.jpg"},
	)}},

	// 任何厂商的思考块都丢弃；只剩思考块的 assistant 消息整条跳过
	"10_thinking_dropped": {Model: "deepseek-chat", Messages: []ir.Message{
		user(text("2+2?")),
		assistant(ir.ThinkingPart{Vendor: ir.VendorAnthropic, Text: "simple sum", Signature: "sig-1"}, text("4")),
		user(text("3+3?")),
		assistant(ir.ThinkingPart{Vendor: ir.VendorDeepSeek, Text: "deepseek reasoning"}),
		user(text("Hello?")),
	}},

	// tool_choice=none 保留 tools；无参数工具补空 schema
	"13_tool_choice_none": {Model: "deepseek-chat",
		Tools: []ir.Tool{{Name: "noop"}}, ToolChoice: &ir.ToolChoice{Mode: "none"},
		Messages: []ir.Message{user(text("Just chat"))},
	},

	// 采样参数、user、stream（带 include_usage）、tool_choice=any → required
	"14_params": {Model: "deepseek-chat", MaxTokens: 256, Temperature: f64(0.2), TopP: f64(0.9),
		StopSequences: []string{"END"}, Stream: true, User: "u-42",
		Tools: []ir.Tool{weatherTool}, ToolChoice: &ir.ToolChoice{Mode: "any"},
		Messages: []ir.Message{user(text("Go"))},
	},

	// 没有 tools 时不发送 tool_choice
	"15_tool_choice_without_tools": {Model: "deepseek-chat", ToolChoice: &ir.ToolChoice{Mode: "auto"},
		Messages: []ir.Message{user(text("Hi"))}},
}

func TestSynthesizeOpenAIChatGolden(t *testing.T) {
	for name, req := range chatSynthCases {
		t.Run(name, func(t *testing.T) {
			b, err := synthesizeOpenAIChat(req)
			if err != nil {
				t.Fatalf("synthesizeOpenAIChat() error = %v", err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, b, "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			path := filepath.Join("testdata", "ir_to_chat", name, "expected.json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			compareGolden(t, path, pretty.Bytes())
		})
	}
}

func TestSynthesizeOpenAIChatRejects(t *testing.T) {
	tests := []struct {
		name      string
		req       *ir.Request
		wantInMsg string
	}{
		{"too many stops", &ir.Request{Model: "m", StopSequences: []string{"a", "b", "c", "d", "e"}, Messages: []ir.Message{user(text("hi"))}}, "stop_sequences"},
		{"image in tool result", &ir.Request{Model: "m", Messages: []ir.Message{
			user(ir.ToolResultPart{ToolCallID: "t", Content: []ir.Part{ir.ImagePart{URL: "https://x/y.png"}}})}}, "images in tool results"},
		{"empty image", &ir.Request{Model: "m", Messages: []ir.Message{user(text("a"), ir.ImagePart{})}}, "neither data nor url"},
		{"no messages", &ir.Request{Model: "m", Messages: []ir.Message{assistant(ir.ThinkingPart{Vendor: ir.VendorOpenAI})}}, "empty"},
		{"bad tool_choice", &ir.Request{Model: "m", Tools: []ir.Tool{{Name: "f"}}, ToolChoice: &ir.ToolChoice{Mode: "sometimes"},
			Messages: []ir.Message{user(text("hi"))}}, "tool_choice"},
		{"tool result in assistant", &ir.Request{Model: "m", Messages: []ir.Message{assistant(ir.ToolResultPart{ToolCallID: "t"})}}, "assistant"},
		{"tool call in user", &ir.Request{Model: "m", Messages: []ir.Message{user(text("a"), ir.ToolCallPart{ID: "t", Name: "f"})}}, "user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := synthesizeOpenAIChat(tt.req)
			if !isInvalid(err) {
				t.Fatalf("error = %v, want invalid_request", err)
			}
			if !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("error %q should mention %q", err, tt.wantInMsg)
			}
		})
	}
}

// 用户消息中只有思考块（不应出现，但 IR 允许）：整条跳过而不是发送空数组。
func TestSynthesizeOpenAIChatSkipsThinkingOnlyUser(t *testing.T) {
	b, err := synthesizeOpenAIChat(&ir.Request{Model: "m", Messages: []ir.Message{
		user(text("hi")), user(ir.ThinkingPart{Vendor: ir.VendorAnthropic}, ir.ThinkingPart{Vendor: ir.VendorAnthropic})}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Messages) != 1 {
		t.Fatalf("messages = %s (err %v)", b, err)
	}
}
