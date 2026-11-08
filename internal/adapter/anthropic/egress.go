package anthropic

import (
	"context"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// DefaultVersion 是客户端未提供 anthropic-version 时使用的值（HDR-03）。
const DefaultVersion = "2023-06-01"

// Egress 是 Anthropic 出站适配器（网关 → Anthropic 上游）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回 Anthropic 报告的 4 种 token 单位。
func (Egress) Units() []string {
	return []string{pricing.UnitInputTokens, pricing.UnitOutputTokens, pricing.UnitCacheReadTokens, pricing.UnitCacheWriteTokens}
}

// Prepare 实现同协议透传（PT-01、PT-02、PT-04）。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	body, err := prepareBody(env.Body, upstreamModel)
	if err != nil {
		return nil, err
	}
	return &protocol.Call{Op: env.Op, Model: upstreamModel, Stream: env.Stream, Body: body, Header: env.Header}, nil
}

// Do 执行一次尝试：发送 POST /v1/messages，按响应类型返回事件流或缓冲体（spec/08 §3）。
func (e Egress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	if call.Op != OpMessages {
		return nil, apperr.New(apperr.CodeInvalidRequest, "anthropic upstream does not support operation "+call.Op)
	}
	req, err := e.newRequest(ctx, up.BaseURL, up.Secret, call.Body, call.Header)
	if err != nil {
		return nil, err
	}
	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}
	m := &meter{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return &protocol.Response{
			Kind:   protocol.ResponseEvents,
			Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Cache-Control": {"no-cache"}},
			Events: protocol.NewSSEStream(resp.Body, func(ev sse.Event) (bool, error) {
				m.observeEvent(ev)
				if o, isErr := classifyStreamError(ev); isErr {
					m.fault, m.hasFault = o, true
				}
				return true, nil // FWD-01：全部转发
			}),
			Meter: m,
		}, nil
	}
	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}
	m.observeBody(body)
	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type")
	return &protocol.Response{Kind: protocol.ResponseBuffered, Status: resp.StatusCode, Header: h, Body: body, Meter: m}, nil
}

// prepareBody 实现 PT-01、PT-02、PT-04。
func prepareBody(body []byte, upstreamModel string) ([]byte, error) {
	// TODO(T09A): 用 jsonx.SetTopLevelString 改写 model；非 JSON 对象返回 apperr.CodeInvalidRequest。
	return nil, apperr.NotImplemented("T09A")
}

// newRequest 实现 HDR-01 ~ HDR-03、HDR-05。
func (e Egress) newRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error) {
	// TODO(T09A): URL = protocol.JoinURL(baseURL, "/v1/messages")；只复制白名单头部。
	return nil, apperr.NotImplemented("T09A")
}

// classifyStreamError 实现 STR-05：ev.Name == "error" 时解析 data 中的 error.type。
func classifyStreamError(ev sse.Event) (protocol.Outcome, bool) {
	// TODO(T09A)
	return protocol.Outcome{}, false
}

// meter 累积 Anthropic 响应中的 usage（USE-01、USE-02）。实现 protocol.Meter。
type meter struct {
	usage     pricing.Usage
	haveInput bool  // 收到了 message_start（或非流式响应体）中的 usage
	haveFinal bool  // 收到了 message_delta 中的最终 output_tokens（或非流式响应体）
	textBytes int64 // 已观察到的 text_delta 字节数，用于 USE-07 估算

	fault    protocol.Outcome
	hasFault bool
}

func (m *meter) observeEvent(ev sse.Event) {
	// TODO(T09A): 处理 message_start（data.message.usage）、message_delta（data.usage）、
	// content_block_delta 的 text 字节数（USE-01、USE-07）。
}

func (m *meter) observeBody(body []byte) {
	// TODO(T09A): 解析顶层 usage（USE-02）。
}

// Usage 实现 USE-07：完整 → UsageUpstream；只有输入 → 输出按 textBytes 估算，UsageEstimated；都没有 → UsageNone。
func (m *meter) Usage() (pricing.Usage, protocol.UsageSource) {
	switch {
	case m.haveInput && m.haveFinal:
		return m.usage, protocol.UsageUpstream
	case m.haveInput:
		u := pricing.Usage{}
		for k, v := range m.usage {
			u[k] = v
		}
		u[pricing.UnitOutputTokens] = max(u[pricing.UnitOutputTokens], pricing.EstimateOutputTokens(m.textBytes))
		return u, protocol.UsageEstimated
	default:
		return nil, protocol.UsageNone
	}
}

func (m *meter) Fault() (protocol.Outcome, bool) { return m.fault, m.hasFault }
