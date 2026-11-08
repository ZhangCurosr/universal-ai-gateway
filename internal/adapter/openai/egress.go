package openai

import (
	"context"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Egress 是 OpenAI 兼容出站适配器（网关 → OpenAI / DeepSeek / Ollama ……）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回 OpenAI 报告的 token 单位（OpenAI 没有缓存写入计费）。
func (Egress) Units() []string {
	return []string{pricing.UnitInputTokens, pricing.UnitOutputTokens, pricing.UnitCacheReadTokens}
}

// callState 是 Call.State：记录 Prepare 是否为了拿到用量而注入了 stream_options（PT-03、FWD-02）。
type callState struct{ injectedUsage bool }

// Prepare 实现同协议透传（PT-01 ~ PT-03）。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	body, injected, err := prepareBody(env.Body, upstreamModel, env.Stream)
	if err != nil {
		return nil, err
	}
	return &protocol.Call{
		Op: env.Op, Model: upstreamModel, Stream: env.Stream, Body: body, Header: env.Header,
		State: callState{injectedUsage: injected},
	}, nil
}

// Do 执行一次尝试：发送 POST /v1/chat/completions，按响应类型返回事件流或缓冲体（spec/08 §3）。
// 跨协议转译生成的 Call 的 State 可以为 nil（视为未注入）。
func (e Egress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	if call.Op != OpChatCompletions {
		return nil, apperr.New(apperr.CodeInvalidRequest, "openai upstream does not support operation "+call.Op)
	}
	st, _ := call.State.(callState)
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
				return shouldForward(ev, st.injectedUsage), nil
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

// prepareBody 实现 PT-01 ~ PT-03。
func prepareBody(body []byte, upstreamModel string, stream bool) ([]byte, bool, error) {
	// TODO(T09B): 改写 model；stream 时按 PT-03 注入 stream_options.include_usage。
	return nil, false, apperr.NotImplemented("T09B")
}

// newRequest 实现 HDR-01、HDR-02、HDR-04、HDR-05。
func (e Egress) newRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error) {
	// TODO(T09B): URL = protocol.JoinURL(baseURL, "/v1/chat/completions")；Authorization: Bearer <secret>。
	return nil, apperr.NotImplemented("T09B")
}

// shouldForward 实现 FWD-02、FWD-03。
func shouldForward(ev sse.Event, injectedUsage bool) bool {
	// TODO(T09B): injectedUsage 时过滤「choices 为空且 usage 非 null」的 chunk。
	return true
}

// meter 累积 OpenAI 响应中的 usage（USE-03 ~ USE-06）。实现 protocol.Meter。
// OpenAI 流中没有标准错误事件，Fault 恒为 false。
type meter struct {
	usage     pricing.Usage
	haveUsage bool
	textBytes int64 // 已观察到的 delta.content 字节数，用于 USE-07 估算
}

func (m *meter) observeEvent(ev sse.Event) {
	// TODO(T09B): 忽略 [DONE]；解析 data JSON 中的顶层 usage（USE-03 ~ USE-05）；累计 choices[*].delta.content 字节数。
}

func (m *meter) observeBody(body []byte) {
	// TODO(T09B)
}

// Usage 实现 USE-07：有 usage → UsageUpstream；没有但有文本 → 只估算输出，UsageEstimated；都没有 → UsageNone。
func (m *meter) Usage() (pricing.Usage, protocol.UsageSource) {
	switch {
	case m.haveUsage:
		return m.usage, protocol.UsageUpstream
	case m.textBytes > 0:
		return pricing.Usage{pricing.UnitOutputTokens: pricing.EstimateOutputTokens(m.textBytes)}, protocol.UsageEstimated
	default:
		return nil, protocol.UsageNone
	}
}

func (m *meter) Fault() (protocol.Outcome, bool) { return protocol.Outcome{}, false }
