package sse

import (
	"bytes"
	"io"
	"testing"
)

// BenchmarkReader 测试 SSE 解析器解析 1000 个事件的吞吐（spec/02 §10.1 BEN-01，spec/13 §4）。
func BenchmarkReader(b *testing.B) {
	b.ReportAllocs()

	// 构造 1000 个事件，每个事件约 200 字节
	const singleEventData = `{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"payload text for benchmarking reader"},"finish_reason":null}]}`
	var buf bytes.Buffer
	for i := 0; i < 1000; i++ {
		buf.WriteString("event: message\ndata: ")
		buf.WriteString(singleEventData)
		buf.WriteString("\n\n")
	}
	payload := buf.Bytes()
	b.SetBytes(int64(len(payload)))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := NewReader(bytes.NewReader(payload))
		count := 0
		for {
			_, err := r.Next()
			if err != nil {
				if err == io.EOF {
					break
				}
				b.Fatalf("Next error at %d: %v", count, err)
			}
			count++
		}
		if count != 1000 {
			b.Fatalf("got %d events, want 1000", count)
		}
	}
}
