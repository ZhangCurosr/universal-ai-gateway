// Package mineru 实现 MinerU 文档解析 API（POST /v1/parse 同步封装与 POST /v1/tasks 异步透传）的入站与出站适配器，
// 并把文档解析能力暴露为工具 mineru_parse（spec/08 §8.2，任务卡 T31）。
//
//	ingress.go  客户端 → 网关：路由、解析请求、错误格式、工具契约（ToolSource）
//	egress.go   网关 → 上游：任务提交、状态轮询、结果下载、用量提取（pages、requests）
//
// 本包不得导入其他适配器包（ARCH-03），也不得导入内核（ARCH-04）。
package mineru

import "github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"

// Protocol 是本适配器的协议名，即配置文件 pools[].protocol 的取值。
const Protocol protocol.Protocol = "mineru"

// 操作名。
const (
	OpParse      = "parse"       // 同步封装：POST /v1/parse（提交 + 轮询 + 下载）
	OpTaskCreate = "task.create" // 异步透传：POST /v1/tasks（提交）
	OpTaskGet    = "task.get"    // 异步透传：GET /v1/tasks/{id}（查询）
	OpTaskResult = "task.result" // 异步透传：GET /v1/tasks/{id}/result（下载）
	OpParseFile  = "parse.file"  // 本地文件同步封装：POST /v1/parse/file（上传 + 轮询 + 下载，MU-07）
	OpTaskUpload = "task.upload" // 本地文件异步：POST /v1/tasks/file（上传，MU-07）
)

// 服务名默认值（IN-G03，spec/08 §8.2）。
const (
	ModelParse = "mineru-parse"
)

// 计量单位名（spec/06 §1，spec/08 §8.2）。
const (
	UnitPages    = "pages"
	UnitRequests = "requests"
)

// Register 把本包的入站与出站适配器注册到 r。
func Register(r *protocol.Registry) error {
	if err := r.RegisterIngress(Ingress{}, protocol.IngressOptions{Priority: 10}); err != nil {
		return err
	}
	return r.RegisterEgress(Egress{})
}
