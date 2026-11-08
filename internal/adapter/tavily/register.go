// Package tavily 实现 Tavily 搜索 API（POST /search、POST /extract、POST /crawl）的入站与出站适配器，
// 并把搜索与网页正文提取能力暴露为工具 tavily_search、tavily_extract（spec/08 §8.1，任务卡 T30）。
//
//	ingress.go  客户端 → 网关：路由、解析请求、错误格式、工具契约（ToolSource）
//	egress.go   网关 → 上游：请求改写（剔除客户端 api_key）、端点调用、用量提取（credits）
//
// 本包不得导入其他适配器包（ARCH-03），也不得导入内核（ARCH-04）。
package tavily

import "github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"

// Protocol 是本适配器的协议名，即配置文件 pools[].protocol 的取值。
const Protocol protocol.Protocol = "tavily"

// 操作名。
const (
	OpSearch  = "search"
	OpExtract = "extract"
	OpCrawl   = "crawl"
)

// 服务名默认值（IN-G03）。
const (
	ModelSearch  = "tavily-search"
	ModelExtract = "tavily-extract"
	ModelCrawl   = "tavily-crawl"
)

// 计量单位名（spec/06 §1，spec/08 §8.1）。
const UnitCredits = "credits"

// Register 把本包的入站与出站适配器注册到 r。
func Register(r *protocol.Registry) error {
	if err := r.RegisterIngress(Ingress{}, protocol.IngressOptions{Priority: 10}); err != nil {
		return err
	}
	return r.RegisterEgress(Egress{})
}
