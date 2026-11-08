package protocol

// Translator 把一种入站协议转换为另一种上游协议（spec/10）。
// 内核只在 入站协议 ≠ 上游协议 时使用它；同协议永远走 Egress.Prepare 透传。
// 实现必须无状态、并发安全。
type Translator interface {
	From() Protocol // 入站协议
	To() Protocol   // 上游协议

	// Request 把入站请求转换为上游协议的 Call（随后交给上游协议的 Egress.Do），服务名设为 upstreamModel。
	// 必须保证上游会返回用量（例如为 OpenAI 流式请求设置 stream_options.include_usage）。
	// 无法表达的特性或不支持的操作返回 CodeInvalidRequest（spec/10 §1 第 2 条）。
	Request(env *Envelope, upstreamModel string) (*Call, error)

	// Response 把上游协议的响应转换为入站协议的响应：
	//   - ResponseBuffered：转换 Body；
	//   - ResponseEvents：返回包装了 resp.Events 的新 EventStream（逐个转换，结束时追加收尾事件）；
	//   - ResponseRaw：通常原样返回（字节流没有协议结构）。
	// 返回值的 Meter 与 Pins 被内核忽略：用量与固定始终取自上游原始响应（CORE-04）。
	Response(env *Envelope, resp *Response) (*Response, error)
}
