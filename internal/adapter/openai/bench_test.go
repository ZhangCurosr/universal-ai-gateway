package openai

import (
	"fmt"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// BenchmarkMeterStream 测试在 200 个 SSE 事件流上提取用量与统计开销（spec/02 §10.1 BEN-01，spec/08 §3）。
func BenchmarkMeterStream(b *testing.B) {
	b.ReportAllocs()

	events := make([]sse.Event, 0, 200)
	for i := 0; i < 199; i++ {
		events = append(events, sse.Event{
			Data: []byte(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"token_%d "},"finish_reason":null}]}`, i)),
		})
	}
	events = append(events, sse.Event{
		Data: []byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":20}}}`),
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := &meter{}
		for j := range events {
			m.observeEvent(events[j])
		}
		u, src := m.Usage()
		if src != protocol.UsageUpstream || u[pricing.UnitInputTokens] != 80 {
			b.Fatalf("unexpected usage: %+v src: %v", u, src)
		}
	}
}
