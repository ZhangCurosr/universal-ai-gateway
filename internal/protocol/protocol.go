// Package protocol 定义内核与协议适配器之间的全部契约（spec/08）。
//
// 架构：
//
//	入站适配器 (Ingress)  →  内核 (internal/core)  →  出站适配器 (Egress)
//	          ↘——— 跨协议时经 Translator 转换 ———↙
//
// 「协议」指任意一种 HTTP API 的形状：大模型 API（Anthropic Messages、OpenAI Chat），
// 也可以是搜索 API、文档解析 API、语音 API 等非大模型 API。契约不假设请求里有 messages、
// 不假设按 token 计费、不假设一次请求只对应一次上游调用，也不假设响应是 JSON：
//
//   - 一个入站协议可以声明任意多个端点，每个端点是一个「操作」（Route.Op）；
//   - 请求体可以缓冲、流式透传（大文件上传）或没有（GET 查询）（Route.Body）；
//   - 用量是任意计量单位的集合（pricing.Usage：token、次数、页数……）；
//   - 一次尝试可以包含多次上游 HTTP 调用（例如异步任务的提交 + 轮询），由 Egress.Do 封装；
//   - 响应可以是缓冲体、SSE 事件流或原样字节流（Response.Kind）；
//   - 异步任务可以把任务 ID 固定到创建它的上游 Key（Response.Pins + AffinityHint.Strict）。
//
// 内核只依赖本包的接口，永远不导入任何具体适配器（ARCH-07）。新增协议 = 新增一个
// internal/adapter/<name> 包实现这些接口 + 在 cmd/gateway/adapters.go 中注册一行。
//
// 本包是冻结接口：修改任何导出签名都需要架构负责人批准（spec/13 §7）。
package protocol

import (
	"io"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
)

// Protocol 是协议名，如 "anthropic"、"openai_chat"、"tavily"。由适配器自行定义，内核不枚举。
type Protocol string

// Envelope 是入站请求解析后的协议中立信封。内核只通过它了解请求，不读取请求体字段。
type Envelope struct {
	// Protocol 是入站协议，由 Ingress.Parse 填写为 Ingress.Protocol()。
	Protocol Protocol
	// Op 是操作名，由 Ingress.Parse 填写为命中路由的 Route.Op。
	Op string
	// Model 是客户端请求的服务名（客户端可见名，尚未映射），对应配置 models[].name。
	// 大模型 API 是模型名；没有模型概念的 API 由入站适配器给出固定名或从请求中取（如 "tavily-search"）。
	Model string
	// Stream 表示客户端要求流式响应。
	Stream bool
	// Estimate 是本请求用量的保守上界估计，用于预留额度（spec/06 PRC-03）。可以只含部分单位；
	// 内核会用模型配置的 reserve 补齐与封顶。没有可估计的单位时留空。
	Estimate pricing.Usage
	// Affinity 是会话亲和线索；零值 = 无线索（内核会退回请求头或不做亲和）。
	Affinity AffinityHint
	// Params 是适配器从路径、查询串、请求头中提取的参数（如任务 ID），供同协议的 Egress 使用。
	// 键名由适配器自行约定；内核不读取。
	Params map[string]string
	// Body 是缓冲的原始请求体（Route.Body == BodyBuffered 时）。适配器与内核都不得修改它。
	Body []byte
	// BodyStream 是未缓冲的请求体（Route.Body == BodyStream 时），由内核在 Parse 之后填写，
	// 已按上限包装；只能被读取一次，因此这类请求不会重试（CORE-07）。
	BodyStream io.Reader
	// Header 是入站请求头（只读）。
	Header http.Header
}

// AffinityHint 是入站适配器从请求中提取的会话线索（spec/07 §7）。内核负责对 Value 做哈希。
type AffinityHint struct {
	// Source 取 AffinityUser、AffinityPrefix 或 AffinityPin。
	Source string
	// Value 是参与哈希的原始字节；为空表示无线索。
	Value []byte
	// Strict 为 true 时，线索必须命中一个已记录的 Key，且只能使用该 Key（不换 Key 重试）；
	// 未命中返回 404 not_found。用于异步任务的查询（任务只存在于创建它的上游账号下）。
	Strict bool
}

const (
	AffinityUser   = "u" // 客户端显式给出的会话 / 用户标识
	AffinityPrefix = "p" // 提示词前缀
	AffinityPin    = "j" // 由 Response.Pins 记录的任务 ID（spec/07 §7.1）
)

// ModelInfo 是目录端点（RouteCatalog）中的一项。
type ModelInfo struct {
	ID       string
	Protocol Protocol // 该模型所在池的上游协议
}

// RouteKind 是入站路由的用途。
type RouteKind int

const (
	// RouteCall 是调用端点（如 POST /v1/messages、POST /search），由内核处理。
	RouteCall RouteKind = iota
	// RouteCatalog 是目录端点（如 GET /v1/models），由 Ingress.WriteCatalog 输出。
	RouteCatalog
)

// BodyMode 是内核如何读取请求体。
type BodyMode int

const (
	// BodyBuffered：读入内存（上限见 Route.MaxBodyBytes），传给 Parse，可以重试。
	BodyBuffered BodyMode = iota
	// BodyStream：不读入内存，Parse 收到 nil，之后以 Envelope.BodyStream 交给出站适配器；只尝试一次。
	// 用于大文件上传。
	BodyStream
	// BodyNone：请求没有请求体（如 GET 查询任务状态）。Parse 收到 nil。
	BodyNone
)

// Route 是入站适配器声明的一个 HTTP 端点。Path 使用 http.ServeMux 的模式语法（可含 {name} 通配段）。
type Route struct {
	Method string
	Path   string
	Kind   RouteKind
	// Op 是操作名（RouteCall 必填，同一入站协议内唯一），如 "messages"、"search"、"task.submit"。
	Op string
	// Body 是请求体读取方式。
	Body BodyMode
	// MaxBodyBytes 是本端点的请求体上限；0 = 使用全局 limits 的值。最终上限取两者中较小者（CORE-06）。
	MaxBodyBytes int64
}
