package tavily

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// Egress 是 Tavily 出站适配器（网关 → Tavily 上游）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回 Tavily 可能报告的计量单位（EG-01，spec/08 §8.1）。
func (Egress) Units() []string {
	return []string{UnitCredits}
}

// Prepare 实现同协议透传与请求体改写（剔除客户端 api_key，spec/08 §8.1）。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	body, err := prepareBody(env.Body)
	if err != nil {
		return nil, err
	}

	// spec/08 §8.1：只有 search 有估计用量；extract / crawl 为 0，由 reserve 决定。
	var estCredits int64
	if env.Op == OpSearch {
		estCredits = 1
		if depth, ok := jsonx.GetString(env.Body, "search_depth"); ok && depth == "advanced" {
			estCredits = 2
		}
	}

	return &protocol.Call{
		Op:     env.Op,
		Model:  upstreamModel,
		Stream: env.Stream,
		Body:   body,
		Header: env.Header,
		Params: env.Params,
		State:  estCredits,
	}, nil
}

// Do 执行一次尝试：发送 POST <base_url>/<op> 并返回响应（spec/08 §3、§8.1）。
func (e Egress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	switch call.Op {
	case OpSearch, OpExtract, OpCrawl:
	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "tavily upstream does not support operation "+call.Op)
	}

	reqURL := protocol.JoinURL(up.BaseURL, "/"+call.Op)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(call.Body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+string(up.Secret))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	ua := e.UserAgent
	if ua == "" {
		ua = "universal-ai-gateway"
	}
	req.Header.Set("User-Agent", ua)

	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}

	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}

	var (
		usage pricing.Usage
		src   protocol.UsageSource
	)
	if c, ok := extractCredits(body); ok {
		usage = pricing.Usage{UnitCredits: c}
		src = protocol.UsageUpstream
	} else if est, _ := call.State.(int64); est > 0 {
		usage = pricing.Usage{UnitCredits: est}
		src = protocol.UsageEstimated
	} else {
		// spec/08 §8.1：extract / crawl 没有估计用量——UsageNone 让内核按 reserve 结算，
		// 而不是按 1 credit 少计费（Tavily 按 URL 批次计费）。
		src = protocol.UsageNone
	}

	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type")

	m := &meter{usage: usage, src: src}
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: resp.StatusCode,
		Header: h,
		Body:   body,
		Meter:  m,
	}, nil
}

// prepareBody 校验 JSON 对象并剔除客户端的 api_key（不修改原切片，CONF-E06）。
func prepareBody(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}
	delete(m, "api_key")
	return encodeObject(m)
}

// encodeObject 保持原始字节输出 JSON 对象，按键名排序。
func encodeObject(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := enc.Encode(k); err != nil {
			return nil, err
		}
		buf.Truncate(buf.Len() - 1)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(m[k]))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// extractCredits 从响应体中提取用量 credits。
func extractCredits(body []byte) (int64, bool) {
	for _, path := range [][]string{{"usage", "credits"}, {"credits"}, {"usage"}} {
		raw, ok := jsonx.Get(body, path...)
		if !ok {
			continue
		}
		var n int64
		if err := json.Unmarshal(raw, &n); err == nil && n >= 0 {
			return n, true
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil && f >= 0 {
			return int64(f), true
		}
	}
	return 0, false
}

// meter 报告 Tavily 调用的用量。
type meter struct {
	usage pricing.Usage
	src   protocol.UsageSource
}

func (m *meter) Usage() (pricing.Usage, protocol.UsageSource) {
	return m.usage, m.src
}

func (m *meter) Fault() (protocol.Outcome, bool) {
	return protocol.Outcome{}, false
}
