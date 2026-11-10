package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Egress 是 Gemini 出站适配器（网关 → Google AI Studio）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回 Gemini 报告的 token 单位（EG-G-05）。
func (Egress) Units() []string {
	return []string{pricing.UnitInputTokens, pricing.UnitOutputTokens, pricing.UnitCacheReadTokens}
}

// Prepare 实现同协议透传（PT-G-01 ~ PT-G-02）：请求体原样作为上游请求体。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	if _, err := jsonx.Object(env.Body); err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}
	return &protocol.Call{
		Op:     env.Op,
		Model:  upstreamModel,
		Stream: env.Stream,
		Params: env.Params,
		Body:   env.Body,
		Header: env.Header,
	}, nil
}

// Do 执行一次尝试：发送 POST /v1beta/models/{model}:{method}（spec/08 §10.3）。
func (e Egress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	if call.Op != OpModelsCall {
		return nil, apperr.New(apperr.CodeInvalidRequest, "gemini upstream does not support operation "+call.Op)
	}

	method := "generateContent"
	if call.Params != nil && call.Params["method"] == "countTokens" {
		method = "countTokens"
	} else if call.Stream {
		method = "streamGenerateContent"
	}

	path := "/v1beta/models/" + url.PathEscape(call.Model) + ":" + method
	if method == "streamGenerateContent" {
		path += "?alt=sse"
	}
	u := protocol.JoinURL(up.BaseURL, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(call.Body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "build upstream request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("x-goog-api-key", string(up.Secret))
	ua := e.UserAgent
	if ua == "" {
		ua = "universal-ai-gateway"
	}
	req.Header.Set("User-Agent", ua)

	resp, err := protocol.Send(up, req)
	if err != nil {
		var ae *protocol.AttemptError
		if errors.As(err, &ae) && len(ae.Body) > 0 {
			if d := extractRetryDelay(ae.Body); d > 0 {
				if d > ae.Outcome.RetryAfter {
					ae.Outcome.RetryAfter = d
				}
			}
		}
		return nil, err
	}

	m := &meter{isCountTokens: method == "countTokens"}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return &protocol.Response{
			Kind:   protocol.ResponseEvents,
			Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Cache-Control": {"no-cache"}},
			Events: protocol.NewSSEStream(resp.Body, func(ev sse.Event) (bool, error) {
				m.observeEvent(ev)
				return true, nil // FWD-G-01: 透传时全部事件原样转发
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
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: resp.StatusCode,
		Header: h,
		Body:   body,
		Meter:  m,
	}, nil
}

// extractRetryDelay 实现 EG-G-04：从上游错误体中提取 RetryInfo 的 retryDelay。
func extractRetryDelay(body []byte) time.Duration {
	details, ok := jsonx.Get(body, "error", "details")
	if !ok {
		return 0
	}
	for i := 0; ; i++ {
		elem, ok := jsonx.ArrayElem(details, i)
		if !ok {
			break
		}
		typ, _ := jsonx.GetString(elem, "@type")
		if strings.HasSuffix(typ, "RetryInfo") {
			if delayStr, ok := jsonx.GetString(elem, "retryDelay"); ok {
				if d := parseDurationStr(delayStr); d > 0 {
					return d
				}
			}
		}
	}
	return 0
}

func parseDurationStr(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	if sec, err := strconv.ParseFloat(s, 64); err == nil && sec > 0 {
		return time.Duration(sec * float64(time.Second))
	}
	return 0
}

// meter 累积 Gemini 响应中的 usage 与流中错误（USE-G-01 ~ USE-G-04、STR-G-01）。
type meter struct {
	usage         pricing.Usage
	haveUsage     bool
	textBytes     int64
	isCountTokens bool
	fault         protocol.Outcome
	haveFault     bool
}

func (m *meter) observeBody(body []byte) {
	if m.isCountTokens {
		return
	}
	if u, ok := jsonx.Get(body, "usageMetadata"); ok {
		m.applyUsage(u)
	}
	m.observeTextBytes(body)
}

func (m *meter) observeEvent(ev sse.Event) {
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return
	}

	// STR-G-01: chunk with error
	if errObj, ok := jsonx.Get(data, "error"); ok {
		code, _ := jsonx.GetInt64(errObj, "code")
		status, _ := jsonx.GetString(errObj, "status")
		var kind protocol.OutcomeKind
		switch {
		case code == 429 || status == "RESOURCE_EXHAUSTED":
			kind = protocol.OutcomeRateLimited
		case code == 503 || status == "UNAVAILABLE":
			kind = protocol.OutcomeOverloaded
		default:
			kind = protocol.OutcomeServerError
		}
		m.fault = protocol.Outcome{Kind: kind, Status: int(code)}
		m.haveFault = true
	}

	if m.isCountTokens {
		return
	}
	if u, ok := jsonx.Get(data, "usageMetadata"); ok {
		m.applyUsage(u)
	}
	m.observeTextBytes(data)
}

// applyUsage 实现 USE-G-01、USE-G-02。
func (m *meter) applyUsage(u json.RawMessage) {
	prompt, ok := jsonx.GetInt64(u, "promptTokenCount")
	if !ok {
		return
	}
	candidates, _ := jsonx.GetInt64(u, "candidatesTokenCount")
	cached, _ := jsonx.GetInt64(u, "cachedContentTokenCount")
	thoughts, _ := jsonx.GetInt64(u, "thoughtsTokenCount")
	toolUse, _ := jsonx.GetInt64(u, "toolUsePromptTokenCount")

	inputTokens := max(prompt-cached, 0) + toolUse
	cacheReadTokens := cached
	outputTokens := candidates + thoughts

	m.usage = pricing.Usage{
		pricing.UnitInputTokens:     inputTokens,
		pricing.UnitOutputTokens:    outputTokens,
		pricing.UnitCacheReadTokens: cacheReadTokens,
	}
	m.haveUsage = true
}

func (m *meter) observeTextBytes(data []byte) {
	cands, ok := jsonx.Get(data, "candidates")
	if !ok {
		return
	}
	for i := 0; ; i++ {
		cand, ok := jsonx.ArrayElem(cands, i)
		if !ok {
			break
		}
		parts, ok := jsonx.Get(cand, "content", "parts")
		if !ok {
			continue
		}
		for j := 0; ; j++ {
			part, ok := jsonx.ArrayElem(parts, j)
			if !ok {
				break
			}
			if text, ok := jsonx.GetString(part, "text"); ok {
				m.textBytes += int64(len(text))
			}
		}
	}
}

// Usage 实现 USE-G-04。
func (m *meter) Usage() (pricing.Usage, protocol.UsageSource) {
	switch {
	case m.isCountTokens:
		return nil, protocol.UsageUpstream
	case m.haveUsage:
		return m.usage, protocol.UsageUpstream
	case m.textBytes > 0:
		return pricing.Usage{pricing.UnitOutputTokens: pricing.EstimateOutputTokens(m.textBytes)}, protocol.UsageEstimated
	default:
		return nil, protocol.UsageNone
	}
}

// Fault 实现 STR-G-01。
func (m *meter) Fault() (protocol.Outcome, bool) {
	if m.haveFault {
		return m.fault, true
	}
	return protocol.Outcome{}, false
}
