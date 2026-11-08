package translate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

func f64(v float64) *float64 { return &v }

func user(parts ...ir.Part) ir.Message      { return ir.Message{Role: ir.RoleUser, Parts: parts} }
func assistant(parts ...ir.Part) ir.Message { return ir.Message{Role: ir.RoleAssistant, Parts: parts} }
func text(s string) ir.TextPart             { return ir.TextPart{Text: s} }

var weatherTool = ir.Tool{
	Name: "get_weather", Description: "Get weather",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
}

// spec/10 §7：IR → anthropic 方向的用例。IR 直接构造，不经过 T20 的解析（§1 第 4 条：每个方向独立测试）。
var anthropicSynthCases = map[string]*ir.Request{
	"01_text": {Model: "up-claude", Messages: []ir.Message{user(text("Hello"))}},

	// 相邻同角色消息合并
	"02_multi_turn_merge": {Model: "up-claude", Messages: []ir.Message{
		user(text("Hi")), user(text("Are you there?")),
		assistant(text("Yes.")),
		user(text("Tell me a joke")),
	}},

	"03_system_cache": {Model: "up-claude",
		System:   []ir.TextPart{{Text: "You are terse."}, {Text: "Long context...", CacheControl: &ir.CacheControl{Type: "ephemeral", TTL: "1h"}}},
		Messages: []ir.Message{user(text("Hi"))},
	},

	"04_tool_result": {Model: "up-claude",
		Tools:      []ir.Tool{weatherTool},
		ToolChoice: &ir.ToolChoice{Mode: "auto"},
		Messages: []ir.Message{
			user(text("Weather in Paris?")),
			assistant(ir.ToolCallPart{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}),
			user(ir.ToolResultPart{ToolCallID: "call_1", Content: []ir.Part{text("18C")}}),
		},
	},

	// 并行两个工具调用；工具结果与随后的 user 文本合并为一条 user 消息
	"05_parallel_tools": {Model: "up-claude",
		Tools:      []ir.Tool{weatherTool},
		ToolChoice: &ir.ToolChoice{Mode: "tool", Name: "get_weather"},
		Messages: []ir.Message{
			user(text("Paris and Rome?")),
			assistant(text("Checking both."),
				ir.ToolCallPart{ID: "call_a", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)},
				ir.ToolCallPart{ID: "call_b", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Rome"}`)}),
			user(ir.ToolResultPart{ToolCallID: "call_a", Content: []ir.Part{text("18C")}},
				ir.ToolResultPart{ToolCallID: "call_b", Content: []ir.Part{text("timeout")}, IsError: true}),
			user(text("Thanks")),
		},
	},

	"06_images": {Model: "up-claude", Messages: []ir.Message{user(
		text("Compare"),
		ir.ImagePart{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
		ir.ImagePart{URL: "https://example.com/cat.jpg"},
	)}},

	// 思考块：Anthropic 签发的保留（含 redacted），其他厂商丢弃；只剩其他厂商思考块的消息整条跳过
	"10_thinking_vendor": {Model: "up-claude", Messages: []ir.Message{
		user(text("2+2?")),
		assistant(ir.ThinkingPart{Vendor: ir.VendorAnthropic, Text: "simple sum", Signature: "sig-1"},
			ir.ThinkingPart{Vendor: ir.VendorAnthropic, Redacted: true, RedactedData: "enc-data"},
			ir.ThinkingPart{Vendor: ir.VendorDeepSeek, Text: "deepseek reasoning"},
			text("4")),
		user(text("3+3?")),
		assistant(ir.ThinkingPart{Vendor: ir.VendorOpenAI, Encrypted: "opaque"}),
		user(text("Hello?")),
	}},

	// 首条消息不是 user：插入 (continue) 占位
	"12_first_assistant": {Model: "up-claude", Messages: []ir.Message{
		assistant(text("Earlier answer")), user(text("Continue")),
	}},

	// tool_choice=none：不发送 tools 与 tool_choice
	"13_tool_choice_none": {Model: "up-claude",
		Tools: []ir.Tool{weatherTool}, ToolChoice: &ir.ToolChoice{Mode: "none"},
		Messages: []ir.Message{user(text("Just chat"))},
	},

	// 采样参数、metadata.user_id、stream；无参数工具补空 schema
	"14_params": {Model: "up-claude", MaxTokens: 256, Temperature: f64(0.2), TopP: f64(0.9),
		StopSequences: []string{"END"}, Stream: true, User: "u-42",
		Tools:      []ir.Tool{{Name: "noop"}},
		ToolChoice: &ir.ToolChoice{Mode: "any"},
		Messages:   []ir.Message{user(text("Go"))},
	},
}

func TestSynthesizeAnthropicGolden(t *testing.T) {
	for name, req := range anthropicSynthCases {
		t.Run(name, func(t *testing.T) {
			b, err := synthesizeAnthropic(req, nil)
			if err != nil {
				t.Fatalf("synthesizeAnthropic() error = %v", err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, b, "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			path := filepath.Join("testdata", "ir_to_anthropic", name, "expected.json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), pretty.Bytes()) {
				t.Errorf("--- got ---\n%s\n--- want ---\n%s", pretty.Bytes(), want)
			}
		})
	}
}

// MaxTokens 为 0 时使用 DefaultMaxTokens（Anthropic 必填）。
func TestSynthesizeAnthropicDefaultMaxTokens(t *testing.T) {
	b, err := synthesizeAnthropic(&ir.Request{Model: "m", Messages: []ir.Message{user(text("hi"))}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MaxTokens int64 `json:"max_tokens"`
	}
	if err := json.Unmarshal(b, &got); err != nil || got.MaxTokens != DefaultMaxTokens {
		t.Fatalf("max_tokens = %d (%v), want %d", got.MaxTokens, err, DefaultMaxTokens)
	}
}

func TestSynthesizeAnthropicRejects(t *testing.T) {
	tests := []struct {
		name      string
		req       *ir.Request
		wantInMsg string
	}{
		{"non-object arguments", &ir.Request{Messages: []ir.Message{user(text("hi")),
			assistant(ir.ToolCallPart{ID: "c", Name: "f", Arguments: json.RawMessage(`[1]`)})}}, "JSON object"},
		{"thinking in tool result", &ir.Request{Messages: []ir.Message{
			user(ir.ToolResultPart{ToolCallID: "c", Content: []ir.Part{ir.ThinkingPart{Vendor: ir.VendorAnthropic}}})}}, "text and images"},
		{"image without media type", &ir.Request{Messages: []ir.Message{user(ir.ImagePart{Data: []byte{1}})}}, "media type"},
		{"empty image", &ir.Request{Messages: []ir.Message{user(ir.ImagePart{})}}, "neither data nor url"},
		{"tool choice without name", &ir.Request{ToolChoice: &ir.ToolChoice{Mode: "tool"}, Messages: []ir.Message{user(text("hi"))}}, "tool name"},
		{"unknown tool choice", &ir.Request{ToolChoice: &ir.ToolChoice{Mode: "sometimes"}, Messages: []ir.Message{user(text("hi"))}}, "sometimes"},
		{"bad role", &ir.Request{Messages: []ir.Message{{Role: "system", Parts: []ir.Part{text("x")}}}}, "role"},
		{"nothing left", &ir.Request{Messages: []ir.Message{assistant(ir.ThinkingPart{Vendor: ir.VendorOpenAI})}}, "nothing left"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := synthesizeAnthropic(tt.req, nil)
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
