// Package provider 定义上游协议适配器接口与上游结果分类（spec/08 §1、§9）。
package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Protocol 是协议标识，取值与 config.Protocol* 常量一致。
type Protocol string

const (
	ProtocolAnthropic  Protocol = "anthropic"
	ProtocolOpenAIChat Protocol = "openai_chat"
)

// OutcomeKind 是一次上游尝试的结果分类（spec/07 §5）。
type OutcomeKind int

const (
	OutcomeSuccess          OutcomeKind = iota // 2xx 且正常结束
	OutcomeClientError                         // 400/404/413/422 等：客户端请求问题，不惩罚 Key
	OutcomeCanceled                            // 客户端断开
	OutcomeLocalError                          // 本机资源错误（端口耗尽等），不惩罚 Key
	OutcomeRateLimited                         // 429
	OutcomeAuthFailed                          // 401
	OutcomePermissionDenied                    // 403
	OutcomeOverloaded                          // 529 / 503：厂商全局过载
	OutcomeServerError                         // 500/502/504 及其他 5xx
	OutcomeTimeout                             // 等待响应头或流空闲超时
	OutcomeNetworkError                        // 连接失败、连接中断
)

var outcomeNames = [...]string{
	"success", "client_error", "canceled", "local_error", "rate_limited", "auth_failed",
	"permission_denied", "overloaded", "server_error", "timeout", "network_error",
}

func (k OutcomeKind) String() string {
	if int(k) >= 0 && int(k) < len(outcomeNames) {
		return outcomeNames[k]
	}
	return "unknown"
}

// Retryable 报告该结果是否允许在首字节之前换 Key 重试（spec/07 §5 表格最后一列）。
func (k OutcomeKind) Retryable() bool {
	switch k {
	case OutcomeRateLimited, OutcomeAuthFailed, OutcomePermissionDenied,
		OutcomeOverloaded, OutcomeServerError, OutcomeTimeout, OutcomeNetworkError:
		return true
	default:
		return false
	}
}

// Outcome 是一次上游尝试的结果。
type Outcome struct {
	Kind       OutcomeKind
	Status     int           // 上游 HTTP 状态码；没有响应时为 0
	RetryAfter time.Duration // 上游建议的等待时长；没有为 0
}

// Adapter 是上游协议适配器。实现必须无状态、并发安全（UsageTap 除外）。
type Adapter interface {
	Protocol() Protocol
	// PrepareBody 对透传请求体做最小改写（spec/08 §3）。
	PrepareBody(body []byte, upstreamModel string, stream bool) (out []byte, injectedUsage bool, err error)
	// NewRequest 构造发往上游的 HTTP 请求（spec/08 §4）。
	NewRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error)
	// NewUsageTap 返回新的 usage 观察器，每个请求一个（spec/08 §5）。
	NewUsageTap() UsageTap
	// ClassifyStreamError 把流中的错误事件映射为 Outcome；不是错误事件返回 ok=false（spec/08 §6.3）。
	ClassifyStreamError(ev sse.Event) (o Outcome, ok bool)
	// ShouldForward 决定一个上游流事件是否转发给客户端（spec/08 §6.2）。
	ShouldForward(ev sse.Event, injectedUsage bool) bool
}

// UsageTap 从上游响应中提取 usage。非并发安全。
type UsageTap interface {
	ObserveEvent(ev sse.Event)
	ObserveBody(body []byte)
	Usage() (pricing.Usage, bool)
}
