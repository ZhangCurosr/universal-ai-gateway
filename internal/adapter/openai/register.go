// Package openai 实现 OpenAI Chat Completions 协议的入站与出站适配器（spec/08，任务卡 T09B）。
// 出站侧同时适用于 DeepSeek、Moonshot、Ollama 等 OpenAI 兼容上游。
//
//	ingress.go  客户端 → 网关：路由、解析请求、错误格式、模型列表
//	egress.go   网关 → 上游：请求改写、鉴权头、用量提取、流事件过滤
//
// 本包不得导入其他适配器包（ARCH-03），也不得导入内核（ARCH-04）。
package openai

import "github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"

// Protocol 是本适配器的协议名，即配置文件 pools[].protocol 的取值。
const Protocol protocol.Protocol = "openai_chat"

// Register 把本包的入站与出站适配器注册到 r。OpenAI 是兜底入站协议（REG-02）。
func Register(r *protocol.Registry) error {
	if err := r.RegisterIngress(Ingress{}, protocol.IngressOptions{Priority: 20, Fallback: true}); err != nil {
		return err
	}
	return r.RegisterEgress(Egress{})
}
