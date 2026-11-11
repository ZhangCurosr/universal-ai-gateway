package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// BenchmarkChatToAnthropicRequest 测试将 ~8 KiB 的 OpenAI Chat 请求转译为 Anthropic 请求的开销（spec/02 §10.1 BEN-01，spec/10 §3）。
func BenchmarkChatToAnthropicRequest(b *testing.B) {
	b.ReportAllocs()

	raw, err := os.ReadFile(filepath.Join("testdata", "chat_to_ir", "01_text", "input.json"))
	if err != nil {
		b.Fatalf("read testdata: %v", err)
	}

	var reqMap map[string]any
	if err := json.Unmarshal(raw, &reqMap); err != nil {
		b.Fatalf("unmarshal testdata: %v", err)
	}

	// 扩充 prompt，使请求体达到约 8 KiB
	reqMap["messages"] = []map[string]any{
		{"role": "user", "content": strings.Repeat("benchmarking translation prompt ", 250)},
	}
	body, err := json.Marshal(reqMap)
	if err != nil {
		b.Fatalf("marshal 8 KiB request: %v", err)
	}
	b.SetBytes(int64(len(body)))

	t := ChatToAnthropic{}
	env := &protocol.Envelope{
		Protocol: protoOpenAIChat,
		Op:       opChatCompletions,
		Model:    "deepseek-chat",
		Body:     body,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		call, err := t.Request(env, "claude-3-5-sonnet")
		if err != nil {
			b.Fatalf("Request failed at %d: %v", i, err)
		}
		if call == nil || len(call.Body) == 0 {
			b.Fatal("call body empty")
		}
	}
}

// BenchmarkAnthropicStreamToChat 测试将 Anthropic 200 个 SSE 事件流转译为 OpenAI Chat SSE 流的开销（spec/02 §10.1 BEN-01，spec/10 §5）。
func BenchmarkAnthropicStreamToChat(b *testing.B) {
	b.ReportAllocs()

	// 构造恰好 200 个合法的 Anthropic 流式事件：
	// 1 message_start + 1 content_block_start + 195 content_block_delta + 1 content_block_stop + 1 message_delta + 1 message_stop = 200
	events := make([]sse.Event, 0, 200)
	events = append(events, sse.Event{
		Name: "message_start",
		Data: []byte(`{"type":"message_start","message":{"id":"msg_bench","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}`),
	})
	events = append(events, sse.Event{
		Name: "content_block_start",
		Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	})
	for i := 0; i < 195; i++ {
		events = append(events, sse.Event{
			Name: "content_block_delta",
			Data: []byte(fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" token_%d"}}`, i)),
		})
	}
	events = append(events, sse.Event{
		Name: "content_block_stop",
		Data: []byte(`{"type":"content_block_stop","index":0}`),
	})
	events = append(events, sse.Event{
		Name: "message_delta",
		Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":196}}`),
	})
	events = append(events, sse.Event{
		Name: "message_stop",
		Data: []byte(`{"type":"message_stop"}`),
	})

	if len(events) != 200 {
		b.Fatalf("expected 200 events, got %d", len(events))
	}

	t := ChatToAnthropic{}
	env := &protocol.Envelope{
		Protocol: protoOpenAIChat,
		Op:       opChatCompletions,
		Model:    "claude-for-chat",
		Body:     []byte(`{"model":"claude-for-chat","stream":true}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		upstream := &sliceStream{evs: append([]sse.Event(nil), events...)}
		resp, err := t.Response(env, &protocol.Response{
			Kind:   protocol.ResponseEvents,
			Events: upstream,
		})
		if err != nil {
			b.Fatalf("Response failed at %d: %v", i, err)
		}
		for {
			_, err := resp.Events.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				b.Fatalf("Next failed: %v", err)
			}
		}
		_ = resp.Events.Close()
	}
}
