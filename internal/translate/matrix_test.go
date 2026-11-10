package translate

import (
	"os"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// TestStreamMatrix 跨协议 Decoder/Encoder 全组合矩阵测试（48 组用例）。
// 覆盖：
//
//	3 个上游 Decoder（Anthropic, Chat, Gemini）
//	× 4 个客户端 Encoder（Chat, Anthropic, Responses, Gemini）
//	× 4 类标准流式用例（文本、工具调用、思考/推理、长度截断）
//	= 48 组用例（T48 由 24 组扩展）
//
// 验证每个组合在流式处理时不报错，且输出事件流严格满足目标客户端协议的结构约束。
func TestStreamMatrix(t *testing.T) {
	fixedTime := time.Unix(1700000000, 0)
	decoders := []struct {
		name    string
		decoder decoder
		cases   []struct {
			name string
			path string
		}
	}{
		{
			name:    "AnthropicUpstream",
			decoder: anthropicDecoder{},
			cases: []struct {
				name string
				path string
			}{
				{"Text", "testdata/anthropic_to_chat/01_text/input.sse"},
				{"TwoTools", "testdata/anthropic_to_chat/05_text_then_two_tools/input.sse"},
				{"Thinking", "testdata/anthropic_to_chat/10_thinking_reasoning/input.sse"},
				{"MaxTokens", "testdata/anthropic_to_chat/15_max_tokens_length/input.sse"},
			},
		},
		{
			name:    "ChatUpstream",
			decoder: chatDecoder{},
			cases: []struct {
				name string
				path string
			}{
				{"Text", "testdata/chat_to_anthropic/01_text/input.sse"},
				{"TwoTools", "testdata/chat_to_anthropic/05_text_then_two_tools/input.sse"},
				{"Reasoning", "testdata/chat_to_anthropic/10_reasoning_dropped/input.sse"},
				{"Length", "testdata/chat_to_anthropic/15_length/input.sse"},
			},
		},
		{
			name:    "GeminiUpstream",
			decoder: geminiDecoder{newID: mockIDGen},
			cases: []struct {
				name string
				path string
			}{
				{"Text", "testdata/gemini_to_chat/01_text/input.sse"},
				{"TwoTools", "testdata/gemini_to_chat/05_text_then_two_tools/input.sse"},
				{"Thinking", "testdata/gemini_to_chat/10_thinking/input.sse"},
				{"MaxTokens", "testdata/gemini_to_chat/15_max_tokens/input.sse"},
			},
		},
	}

	encoders := []struct {
		name     string
		env      *protocol.Envelope
		encoder  encoder
		validate func(t *testing.T, events []sse.Event)
	}{
		{
			name:     "ChatClient",
			env:      &protocol.Envelope{Protocol: protoOpenAIChat, Op: opChatCompletions, Model: "matrix-chat"},
			encoder:  newChatEncoder(fixedTime, mockIDGen),
			validate: checkChatChunks,
		},
		{
			name:     "AnthropicClient",
			env:      &protocol.Envelope{Protocol: protoAnthropic, Op: opAnthropicCreate, Model: "matrix-anthropic"},
			encoder:  newAnthropicEncoder(mockIDGen),
			validate: checkAnthropicEvents,
		},
		{
			name:     "ResponsesClient",
			env:      &protocol.Envelope{Protocol: protoOpenAIResponses, Op: opResponsesCreate, Model: "matrix-responses"},
			encoder:  newResponsesEncoder(fixedTime, mockIDGen),
			validate: checkSequence,
		},
		{
			name:     "GeminiClient",
			env:      &protocol.Envelope{Protocol: protoGemini, Op: opGeminiCall, Model: "matrix-gemini"},
			encoder:  newGeminiEncoder(mockIDGen),
			validate: checkGeminiChunks,
		},
	}

	total := 0
	for _, dec := range decoders {
		for _, enc := range encoders {
			for _, tc := range dec.cases {
				total++
				testName := dec.name + "_" + enc.name + "_" + tc.name
				t.Run(testName, func(t *testing.T) {
					raw, err := os.ReadFile(tc.path)
					if err != nil {
						t.Fatalf("read test input %s: %v", tc.path, err)
					}
					upstream := &sliceStream{evs: readSSE(t, raw)}
					irSt := dec.decoder.Stream(upstream)
					outSt := enc.encoder.Stream(irSt, enc.env)

					got := drainChat(t, outSt)
					enc.validate(t, got)

					if err := outSt.Close(); err != nil {
						t.Errorf("Close() returned error: %v", err)
					}
					if !upstream.closed {
						t.Errorf("upstream stream was not closed")
					}
				})
			}
		}
	}

	if total != 48 {
		t.Fatalf("expected 48 matrix test cases, ran %d", total)
	}
}
