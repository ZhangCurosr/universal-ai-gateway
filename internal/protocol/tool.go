package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
)

// ToolSource 是入站适配器**可选**实现的接口：把本协议的操作暴露为供 AI 调用的工具（spec/14）。
//
// 网关的 MCP 服务端与命令行（gateway call）都通过它工作：工具调用被转换为对本协议某个
// RouteCall 端点的内部请求，交给内核处理（鉴权结果、计费、选 Key、重试与普通请求完全相同），
// 再把该端点的完整响应转换为工具结果。因此工具层不需要知道任何协议细节，协议也不需要知道 MCP。
//
// 实现必须无状态、并发安全。
type ToolSource interface {
	// Tools 返回本协议提供的工具。名称在整个注册表内唯一（REG-05）。
	Tools() []Tool

	// ToolRequest 把工具参数转换为对本协议一个调用端点的请求。
	// 参数不合法时返回 CodeInvalidRequest（工具层会把它作为 isError 的工具结果返回给 AI）。
	ToolRequest(name string, args json.RawMessage) (*ToolRequest, error)

	// ToolResult 把该端点的完整响应转换为工具结果。status 非 2xx 时响应体是本协议的错误格式，
	// 实现应提取错误消息并返回 IsError = true 的结果（而不是返回 error）。
	// 返回 error 仅表示响应无法解析（工具层返回通用的 isError 结果）。
	ToolResult(name string, status int, header http.Header, body []byte) (*ToolResult, error)
}

// Tool 是一个工具的描述（对应 MCP 的 Tool 对象）。
type Tool struct {
	Name        string          // 匹配 ToolNamePattern，如 "tavily_search"
	Title       string          // 人类可读的标题，可选
	Description string          // 给 AI 看的用途说明：什么时候用、返回什么
	InputSchema json.RawMessage // JSON Schema（type 必须是 "object"）
	// Model 是该工具固定使用的服务名（config models[].name）；为空表示由参数指定（如 ask_model 的 model 参数）。
	// 工具列表只列出客户端可用的工具：Model 非空时要求该服务已配置且在客户端白名单内（TL-03）。
	Model string
	// ReadOnly 表示工具不修改外部状态（MCP annotations.readOnlyHint）。
	ReadOnly bool
}

// ToolRequest 是工具调用转换出的内部请求。
type ToolRequest struct {
	Op         string            // 目标调用端点的 Route.Op（必须是本协议的 RouteCall）
	PathValues map[string]string // 路径通配段的值（路由含 {id} 时必填）
	Query      url.Values        // 查询串，可选
	Header     http.Header       // 请求头，可选（Content-Type 缺省为 application/json）
	Body       []byte            // 请求体（BodyNone 路由必须为空）
}

// ToolResult 是工具调用的结果（对应 MCP 的 CallToolResult）。
type ToolResult struct {
	Content []ToolContent
	// Structured 是结构化结果（MCP structuredContent），可选；必须是 JSON 对象。
	Structured json.RawMessage
	IsError    bool
}

// ToolContent 是结果中的一段内容。
type ToolContent struct {
	Type     string // "text" | "image" | "resource"
	Text     string // Type == "text"
	Data     string // Type == "image"：base64 数据
	MIMEType string // Type == "image" / "resource"
	URI      string // Type == "resource"：资源 URI（内容放 Text）
}

// ToolNamePattern 是工具名的格式（MCP 推荐：1–64 个字母、数字、下划线、连字符、点）。
var ToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// TextResult 是只含一段文本的结果的便捷构造函数。
func TextResult(text string, isError bool) *ToolResult {
	return &ToolResult{Content: []ToolContent{{Type: "text", Text: text}}, IsError: isError}
}
