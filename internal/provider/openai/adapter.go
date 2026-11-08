// Package openai 实现 OpenAI Chat Completions 及兼容接口（DeepSeek、Moonshot、Ollama……）的上游适配器
// （任务卡 T09B，spec/08）。
package openai

import (
	"context"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Adapter 是 OpenAI 兼容适配器。零值可用。
type Adapter struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
}

var _ provider.Adapter = Adapter{}

// Protocol 返回 provider.ProtocolOpenAIChat。
func (Adapter) Protocol() provider.Protocol { return provider.ProtocolOpenAIChat }

// PrepareBody 实现 PT-01 ~ PT-03。
func (Adapter) PrepareBody(body []byte, upstreamModel string, stream bool) ([]byte, bool, error) {
	// TODO(T09B): 改写 model；stream 时按 PT-03 注入 stream_options.include_usage。
	return nil, false, apperr.NotImplemented("T09B")
}

// NewRequest 实现 HDR-01、HDR-02、HDR-04、HDR-05。
func (a Adapter) NewRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error) {
	// TODO(T09B): URL = strings.TrimRight(baseURL, "/") + "/v1/chat/completions"；Authorization: Bearer <secret>。
	return nil, apperr.NotImplemented("T09B")
}

// NewUsageTap 返回新的 usage 观察器（USE-03 ~ USE-06）。
func (Adapter) NewUsageTap() provider.UsageTap { return &usageTap{} }

// ClassifyStreamError：OpenAI 流中没有标准错误事件，返回 ok=false。
func (Adapter) ClassifyStreamError(sse.Event) (provider.Outcome, bool) {
	return provider.Outcome{}, false
}

// ShouldForward 实现 FWD-02、FWD-03。
func (Adapter) ShouldForward(ev sse.Event, injectedUsage bool) bool {
	// TODO(T09B): injectedUsage 时过滤「choices 为空且 usage 非 null」的 chunk。
	return true
}

type usageTap struct {
	usage     pricing.Usage
	haveInput bool
}

func (t *usageTap) ObserveEvent(ev sse.Event) {
	// TODO(T09B): 忽略 [DONE]；解析 data JSON 中的顶层 usage（USE-03 ~ USE-05）。
}

func (t *usageTap) ObserveBody(body []byte) {
	// TODO(T09B)
}

func (t *usageTap) Usage() (pricing.Usage, bool) { return t.usage, t.haveInput }
