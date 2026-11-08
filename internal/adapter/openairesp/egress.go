package openairesp

import (
	"context"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Egress 是 OpenAI Responses 出站适配器（网关 → OpenAI / Azure OpenAI / 兼容上游）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回 Responses API 报告的 token 单位。
func (Egress) Units() []string {
	return []string{pricing.UnitInputTokens, pricing.UnitOutputTokens, pricing.UnitCacheReadTokens}
}

// callState 是 Call.State。
type callState struct {
	pin bool // responses.create 且 store != false：把响应 ID 固定到本次 Key（EG-R04）
}

// Prepare 实现同协议透传（PT-R01 ~ PT-R03）。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	call := &protocol.Call{Op: env.Op, Model: upstreamModel, Stream: env.Stream, Header: env.Header, Params: env.Params}
	if env.Op != OpCreate {
		return call, nil
	}
	body, store, err := prepareBody(env.Body, upstreamModel)
	if err != nil {
		return nil, err
	}
	call.Body = body
	call.State = callState{pin: store}
	return call, nil
}

// Do 执行一次尝试（EG-R01 ~ EG-R05）。
func (e Egress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	method, path, ok := upstreamEndpoint(call)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidRequest, "openai_responses upstream does not support operation "+call.Op)
	}
	req, err := e.newRequest(ctx, method, protocol.JoinURL(up.BaseURL, path), up.Secret, call.Body, call.Header)
	if err != nil {
		return nil, err
	}
	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}
	st, _ := call.State.(callState)
	m := &meter{billable: call.Op == OpCreate}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		out := &protocol.Response{
			Kind:   protocol.ResponseEvents,
			Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Cache-Control": {"no-cache"}},
			Meter:  m,
		}
		out.Events = protocol.NewSSEStream(resp.Body, func(ev sse.Event) (bool, error) {
			m.observeEvent(ev)
			if id := m.responseID; st.pin && id != "" && len(out.Pins) == 0 {
				out.Pins = []string{id} // 流式：在 response.created 时追加（CORE-08 在转发结束后读取）
			}
			return true, nil
		})
		return out, nil
	}
	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}
	m.observeBody(body)
	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type")
	out := &protocol.Response{Kind: protocol.ResponseBuffered, Status: resp.StatusCode, Header: h, Body: body, Meter: m}
	if st.pin && m.responseID != "" {
		out.Pins = []string{m.responseID}
	}
	return out, nil
}

// upstreamEndpoint 返回各操作的上游方法与路径（EG-R01）。已实现。
func upstreamEndpoint(call *protocol.Call) (method, path string, ok bool) {
	id := call.Params["id"]
	q := ""
	if raw := call.Params["query"]; raw != "" {
		q = "?" + raw
	}
	switch call.Op {
	case OpCreate:
		return http.MethodPost, "/v1/responses", true
	case OpGet:
		return http.MethodGet, "/v1/responses/" + id + q, id != ""
	case OpDelete:
		return http.MethodDelete, "/v1/responses/" + id, id != ""
	case OpCancel:
		return http.MethodPost, "/v1/responses/" + id + "/cancel", id != ""
	case OpInputItems:
		return http.MethodGet, "/v1/responses/" + id + "/input_items" + q, id != ""
	}
	return "", "", false
}

// prepareBody 实现 PT-R01 ~ PT-R03：改写 model；返回是否需要固定响应 ID（store 缺省为 true）。
func prepareBody(body []byte, upstreamModel string) (out []byte, store bool, err error) {
	// TODO(T09C): jsonx.SetTopLevelString(body, "model", upstreamModel)；非 JSON 对象 → CodeInvalidRequest；
	// store = jsonx.GetBool(body, "store")，不存在时为 true。不注入任何字段（Responses 总是返回 usage）。
	return nil, false, apperr.NotImplemented("T09C")
}

// newRequest 实现 HDR-R01 ~ HDR-R03。
func (e Egress) newRequest(ctx context.Context, method, url string, secret, body []byte, inbound http.Header) (*http.Request, error) {
	// TODO(T09C): body 为空时不设 Content-Type；Authorization: Bearer <secret>；
	// 白名单复制 OpenAI-Beta；User-Agent 同 HDR-05。
	return nil, apperr.NotImplemented("T09C")
}

// meter 实现 USE-R01 ~ USE-R04 与 STR-R01。实现 protocol.Meter。
type meter struct {
	billable   bool // 只有 responses.create 计费；其他操作恒为 (nil, UsageUpstream)
	usage      pricing.Usage
	haveUsage  bool
	textBytes  int64
	responseID string

	fault    protocol.Outcome
	hasFault bool
}

func (m *meter) observeEvent(ev sse.Event) {
	// TODO(T09C): response.created → responseID = data.response.id；
	// response.output_text.delta → textBytes += len(data.delta)；
	// response.completed / response.incomplete / response.failed → usage 取 data.response.usage（USE-R01）；
	// response.failed 与 error 事件 → fault（STR-R01）。
}

func (m *meter) observeBody(body []byte) {
	// TODO(T09C): responseID = 顶层 id；usage 取顶层 usage（USE-R01）。
}

// Usage 实现 USE-R04。
func (m *meter) Usage() (pricing.Usage, protocol.UsageSource) {
	switch {
	case !m.billable:
		return nil, protocol.UsageUpstream
	case m.haveUsage:
		return m.usage, protocol.UsageUpstream
	case m.textBytes > 0:
		return pricing.Usage{pricing.UnitOutputTokens: pricing.EstimateOutputTokens(m.textBytes)}, protocol.UsageEstimated
	default:
		return nil, protocol.UsageNone
	}
}

func (m *meter) Fault() (protocol.Outcome, bool) { return m.fault, m.hasFault }
