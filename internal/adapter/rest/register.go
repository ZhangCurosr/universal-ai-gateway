// Package rest 实现通用 REST 转发适配器（spec/08 §8.3，任务卡 T51）。
// 用于接入无需解析请求体的任意非大模型 REST API（换上游 Key、按次计费、透明转发）。
//
//	register.go  协议名、操作名常量、Register 函数
//	ingress.go   客户端 → 网关：7 条 HTTP 方法路由、路径与参数提取、安全防穿越、错误格式
//	secret.go    上游 Key 格式解析（bearer、header、query、basic）与请求注入
//	egress.go    网关 → 上游：URL 拼接、请求头过滤、Key 注入、透明转发、403 改写、URL query 脱敏与按次计费
//
// 本包不得导入其他适配器包（ARCH-03），也不得导入内核（ARCH-04）。
package rest

import "github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"

// Protocol 是本适配器的协议名，即配置文件 pools[].protocol 的取值。
const Protocol protocol.Protocol = "rest"

// 操作名常量（RS-02）。每个 HTTP 方法在协议内对应唯一 Op（REG-02）。
const (
	OpGet       = "rest.get"
	OpDelete    = "rest.delete"
	OpPut       = "rest.put"
	OpPost      = "rest.post"
	OpPatch     = "rest.patch"
	OpPostIdem  = "rest.post_idem"
	OpPatchIdem = "rest.patch_idem"
)

// 计量单位名（RS-18）。
const UnitRequests = "requests"

// Register 把本包的入站与出站适配器注册到 r。
func Register(r *protocol.Registry) error {
	// RS-01: 注册优先级与 tavily 相同（10），不是 Fallback
	if err := r.RegisterIngress(Ingress{}, protocol.IngressOptions{Priority: 10}); err != nil {
		return err
	}
	return r.RegisterEgress(NewEgress())
}
