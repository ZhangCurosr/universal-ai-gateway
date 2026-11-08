// Package openairesp 实现 OpenAI Responses API（POST /v1/responses 及其后续操作）的入站与出站适配器，
// 并把模型调用暴露为工具 ask_model（spec/08 §9，任务卡 T09C）。
//
//	ingress.go  客户端 → 网关：路由、解析请求、错误格式
//	egress.go   网关 → 上游：请求改写、各操作的上游端点、用量提取、响应 ID 固定
//	tools.go    工具 ask_model（spec/14）
//
// Responses API 是有状态的：store != false 时响应保存在上游账号下，之后的 previous_response_id、
// GET / DELETE / cancel / input_items 都必须使用同一个上游 Key，因此本包大量使用任务固定（spec/07 §7.1）。
//
// 本包不得导入其他适配器包（ARCH-03），也不得导入内核（ARCH-04）。
package openairesp

import "github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"

// Protocol 是本适配器的协议名，即配置文件 pools[].protocol 的取值。
const Protocol protocol.Protocol = "openai_responses"

// 操作名。
const (
	OpCreate     = "responses.create"
	OpGet        = "responses.get"
	OpDelete     = "responses.delete"
	OpCancel     = "responses.cancel"
	OpInputItems = "responses.input_items"
)

// Register 把本包的入站与出站适配器注册到 r。
func Register(r *protocol.Registry) error {
	if err := r.RegisterIngress(Ingress{}, protocol.IngressOptions{Priority: 30}); err != nil {
		return err
	}
	return r.RegisterEgress(Egress{})
}
