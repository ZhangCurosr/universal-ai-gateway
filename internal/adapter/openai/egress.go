package openai

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

// prepareBody 实现 PT-01 ~ PT-03：改写 model；流式且客户端没有要求 usage 时注入 stream_options.include_usage。
func prepareBody(body []byte, upstreamModel string, stream bool) ([]byte, bool, error) {
	out, err := jsonx.SetTopLevelString(body, "model", upstreamModel)
	if err != nil {
		return nil, false, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}
	if !stream {
		return out, false, nil
	}
	if v, ok := jsonx.GetBool(out, "stream_options", "include_usage"); ok && v {
		return out, false, nil
	}
	opts, ok := jsonx.Get(out, "stream_options")
	if !ok {
		opts = json.RawMessage(`{}`)
	}
	opts, err = jsonx.SetTopLevel(opts, "include_usage", json.RawMessage("true"))
	if err != nil {
		return nil, false, apperr.New(apperr.CodeInvalidRequest, "stream_options must be an object")
	}
	if out, err = jsonx.SetTopLevel(out, "stream_options", opts); err != nil {
		return nil, false, apperr.Wrap(apperr.CodeInternal, "rewrite stream_options", err)
	}
	return out, true, nil
}

// newRequest 实现 HDR-01、HDR-02、HDR-04、HDR-05。
func (e Egress) newRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, protocol.JoinURL(baseURL, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "build upstream request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+string(secret))
	ua := e.UserAgent
	if ua == "" {
		ua = "universal-ai-gateway"
	}
	req.Header.Set("User-Agent", ua)
	return req, nil
}

// shouldForward 实现 FWD-02、FWD-03。
func shouldForward(ev sse.Event, injectedUsage bool) bool {
	if !injectedUsage {
		return true
	}
	choices, ok := jsonx.Get(ev.Data, "choices")
	if !ok || !bytes.Equal(bytes.TrimSpace(choices), []byte("[]")) {
		return true
	}
	_, hasUsage := jsonx.Get(ev.Data, "usage")
	return !hasUsage
}

// meter 累积 OpenAI 响应中的 usage（USE-03 ~ USE-06）。实现 protocol.Meter。
// OpenAI 流中没有标准错误事件，Fault 恒为 false。
type meter struct {
	usage     pricing.Usage
	haveUsage bool
	textBytes int64 // 已观察到的 delta.content 字节数，用于 USE-07 估算
}

// apply 实现 USE-03、USE-04。
func (m *meter) apply(u json.RawMessage) {
	prompt, ok := jsonx.GetInt64(u, "prompt_tokens")
	if !ok {
		return
	}
	cached, ok := jsonx.GetInt64(u, "prompt_tokens_details", "cached_tokens")
	if !ok {
		cached, _ = jsonx.GetInt64(u, "prompt_cache_hit_tokens")
	}
	completion, _ := jsonx.GetInt64(u, "completion_tokens")
	m.usage = pricing.Usage{
		pricing.UnitInputTokens:     max(prompt-cached, 0),
		pricing.UnitCacheReadTokens: cached,
		pricing.UnitOutputTokens:    completion,
	}
	m.haveUsage = true
}

func (m *meter) observeEvent(ev sse.Event) {
	if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) { // USE-05
		return
	}
	if u, ok := jsonx.Get(ev.Data, "usage"); ok {
		m.apply(u)
	}
	choices, ok := jsonx.Get(ev.Data, "choices")
	if !ok {
		return
	}
	for i := 0; ; i++ {
		c, ok := jsonx.ArrayElem(choices, i)
		if !ok {
			return
		}
		if text, ok := jsonx.GetString(c, "delta", "content"); ok {
			m.textBytes += int64(len(text))
		}
	}
}

func (m *meter) observeBody(body []byte) {
	if u, ok := jsonx.Get(body, "usage"); ok {
		m.apply(u)
	}
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
