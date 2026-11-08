package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
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

// prepareBody 实现 PT-01、PT-02、PT-04：只改写顶层 model，其余值的字节不变。
func prepareBody(body []byte, upstreamModel string) ([]byte, error) {
	out, err := jsonx.SetTopLevelString(body, "model", upstreamModel)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}
	return out, nil
}

// newRequest 实现 HDR-01 ~ HDR-03、HDR-05。
func (e Egress) newRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, protocol.JoinURL(baseURL, "/v1/messages"), bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "build upstream request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("x-api-key", string(secret))
	version := inbound.Get("anthropic-version")
	if version == "" {
		version = DefaultVersion
	}
	req.Header.Set("anthropic-version", version)
	protocol.CopyHeaders(req.Header, inbound, "anthropic-beta")
	ua := e.UserAgent
	if ua == "" {
		ua = "universal-ai-gateway"
	}
	req.Header.Set("User-Agent", ua)
	return req, nil
}

// classifyStreamError 实现 STR-05：error 事件按 error.type 分类。
func classifyStreamError(ev sse.Event) (protocol.Outcome, bool) {
	if ev.Name != "error" {
		return protocol.Outcome{}, false
	}
	switch t, _ := jsonx.GetString(ev.Data, "error", "type"); t {
	case "overloaded_error":
		return protocol.Outcome{Kind: protocol.OutcomeOverloaded}, true
	case "rate_limit_error":
		return protocol.Outcome{Kind: protocol.OutcomeRateLimited}, true
	default:
		return protocol.Outcome{Kind: protocol.OutcomeServerError}, true
	}
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

// usageFields 是 Anthropic usage 字段到计量单位的映射。
var usageFields = [...]struct{ field, unit string }{
	{"input_tokens", pricing.UnitInputTokens},
	{"output_tokens", pricing.UnitOutputTokens},
	{"cache_read_input_tokens", pricing.UnitCacheReadTokens},
	{"cache_creation_input_tokens", pricing.UnitCacheWriteTokens},
}

// apply 实现 USE-01：存在的字段覆盖之前的值，不存在的保留。返回是否出现了 input_tokens。
func (m *meter) apply(usage json.RawMessage) bool {
	if m.usage == nil {
		m.usage = pricing.Usage{}
	}
	sawInput := false
	for _, f := range usageFields {
		if v, ok := jsonx.GetInt64(usage, f.field); ok {
			m.usage[f.unit] = v
			sawInput = sawInput || f.field == "input_tokens"
		}
	}
	return sawInput
}

func (m *meter) observeEvent(ev sse.Event) {
	switch ev.Name {
	case "message_start":
		if u, ok := jsonx.Get(ev.Data, "message", "usage"); ok && m.apply(u) {
			m.haveInput = true
		}
	case "message_delta":
		if u, ok := jsonx.Get(ev.Data, "usage"); ok {
			if m.apply(u) {
				m.haveInput = true
			}
			m.haveFinal = true
		}
	case "content_block_delta":
		if text, ok := jsonx.GetString(ev.Data, "delta", "text"); ok {
			m.textBytes += int64(len(text))
		}
	}
}

func (m *meter) observeBody(body []byte) {
	if u, ok := jsonx.Get(body, "usage"); ok {
		m.haveInput = m.apply(u)
		m.haveFinal = true
	}
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
