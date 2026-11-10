// Package translate 实现规范化中间表示（IR）与各协议之间的双向转换（spec/10）。
//
// 本包采用解耦的 N + M 架构，分为四类核心组件：
//
// 1. 请求方向（Request Path）：
//   - Lower（客户端协议 → ir.Request）：每个客户端入站协议一个 Lower 实现，
//     将客户端专有请求结构规范化解析为 *ir.Request。
//   - Synthesize（ir.Request → 上游出站请求）：每个上游协议一个 Synthesize 实现，
//     根据出站规范将 *ir.Request 序列化为上游端点所需的请求体与参数。
//
// 2. 响应方向（Response Path）：
//   - Decode（上游响应/流 → ir.Response / ir.StreamEvent 流）：每个上游协议一个 decoder 实现，
//     解析非流式响应体与流式 SSE 增量 chunk，产生统一规范化的 ir.Response 或 ir.StreamEvent 序列。
//   - Encode（ir.Response / ir.StreamEvent 流 → 客户端响应/流）：每个客户端协议一个 encoder 实现，
//     根据客户端协议特征将规范化 IR 编码为客户端非流式 JSON 或 SSE 事件流。
//
// 新协议扩展机制（例如 M3 新增 Gemini 协议）：
//
// 若新增协议兼具客户端与上游能力，只需补充 4 个单向组件，无需为每对协议编写成对状态机：
//
//	Lower:        <name>_lower.go       (协议入站请求 -> ir.Request)
//	Synthesize:   <name>_synth.go       (ir.Request -> 协议出站请求)
//	Decode:       decode_<name>.go      (上游响应/流 -> 响应 IR)
//	Encode:       encode_<name>.go      (响应 IR -> 客户端响应/流)
//
// 并在 register.go 中将 decoder 与 encoder 组合注册到统一的 Translator 实例中。
//
// 架构底线约束：
//
//   - 本包不得导入具体适配器包（spec/10 §0），协议常量集中定义在 protocols.go。
//   - 内核（internal/core）与契约（internal/protocol）保持完全冻结，所有协议转换封装在 internal/translate 内部。
package translate
