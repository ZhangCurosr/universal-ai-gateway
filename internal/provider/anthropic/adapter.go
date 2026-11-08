// Package anthropic 实现 Anthropic Messages API 的上游适配器（任务卡 T09A，spec/08）。
package anthropic

import (
	"context"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// DefaultVersion 是客户端未提供 anthropic-version 时使用的值（HDR-03）。
const DefaultVersion = "2023-06-01"

// Adapter 是 Anthropic 适配器。零值可用。
type Adapter struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
}

var _ provider.Adapter = Adapter{}

// Protocol 返回 provider.ProtocolAnthropic。
func (Adapter) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

// PrepareBody 实现 PT-01、PT-02、PT-04。
func (Adapter) PrepareBody(body []byte, upstreamModel string, stream bool) ([]byte, bool, error) {
	// TODO(T09A): 用 jsonx.SetTopLevelString 改写 model；非 JSON 对象返回 apperr.CodeInvalidRequest。
	return nil, false, apperr.NotImplemented("T09A")
}

// NewRequest 实现 HDR-01 ~ HDR-03、HDR-05。
func (a Adapter) NewRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error) {
	// TODO(T09A): URL = strings.TrimRight(baseURL, "/") + "/v1/messages"；只复制白名单头部。
	return nil, apperr.NotImplemented("T09A")
}

// NewUsageTap 返回新的 usage 观察器（USE-01、USE-02）。
func (Adapter) NewUsageTap() provider.UsageTap { return &usageTap{} }

// ClassifyStreamError 实现 STR-05。
func (Adapter) ClassifyStreamError(ev sse.Event) (provider.Outcome, bool) {
	// TODO(T09A): ev.Name == "error" 时解析 data 中的 error.type。
	return provider.Outcome{}, false
}

// ShouldForward 实现 FWD-01：全部转发。
func (Adapter) ShouldForward(sse.Event, bool) bool { return true }

// usageTap 累积 Anthropic 响应中的 usage。
type usageTap struct {
	usage     pricing.Usage
	haveInput bool
}

func (t *usageTap) ObserveEvent(ev sse.Event) {
	// TODO(T09A): 处理 message_start（data.message.usage）与 message_delta（data.usage），USE-01。
}

func (t *usageTap) ObserveBody(body []byte) {
	// TODO(T09A): 解析顶层 usage。
}

func (t *usageTap) Usage() (pricing.Usage, bool) { return t.usage, t.haveInput }
