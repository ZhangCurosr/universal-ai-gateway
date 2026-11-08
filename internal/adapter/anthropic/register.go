// Package anthropic 实现 Anthropic Messages 协议的入站与出站适配器（spec/08，任务卡 T09A）。
//
//	ingress.go  客户端 → 网关：路由、解析请求、错误格式、模型列表
//	egress.go   网关 → 上游：请求改写、鉴权头、用量提取、流事件分类
//
// 本包不得导入其他适配器包（ARCH-03），也不得导入内核（ARCH-04）。
package anthropic

import "github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"

// Protocol 是本适配器的协议名，即配置文件 pools[].protocol 的取值。
const Protocol protocol.Protocol = "anthropic"

// Register 把本包的入站与出站适配器注册到 r。由 cmd/gateway/adapters.go 调用。
func Register(r *protocol.Registry) error {
	if err := r.RegisterIngress(Ingress{}, protocol.IngressOptions{Priority: 10}); err != nil {
		return err
	}
	return r.RegisterEgress(Egress{})
}
