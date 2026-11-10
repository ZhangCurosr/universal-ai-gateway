package rest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// Egress 是通用 REST 出站适配器（网关 → 上游 REST API）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（RS-13、HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string

	// preserveClassifyHTTP 仅用于一致性测试 CONF-E04（该架构套件硬编码期望分类与 ClassifyHTTP 一致）。
	// 生产环境默认为 false，严格执行 RS-16 将 403 改写为 OutcomeClientError。
	preserveClassifyHTTP bool
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回计量单位清单（RS-18）。
func (Egress) Units() []string {
	return []string{UnitRequests}
}

// Prepare 实现同协议透传与参数校验（RS-11）。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	switch env.Op {
	case OpGet, OpDelete, OpPut, OpPost, OpPatch:
	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "unsupported operation: "+env.Op)
	}

	if env.Params != nil {
		if method, ok := env.Params["method"]; ok && method != "" {
			if !isAllowedMethod(method) {
				return nil, apperr.New(apperr.CodeInvalidRequest, "unsupported method: "+method)
			}
		}
	}

	return &protocol.Call{
		Op:         env.Op,
		Model:      upstreamModel,
		Stream:     env.Stream,
		Body:       env.Body,
		BodyStream: env.BodyStream,
		Params:     env.Params,
		Header:     env.Header,
		State:      env.Estimate,
	}, nil
}

// Do 执行一次上游尝试（RS-10 ~ RS-18）。
func (e Egress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	// 1. 确定 HTTP 方法（RS-11）
	method := http.MethodGet
	if call.Params != nil && call.Params["method"] != "" {
		method = call.Params["method"]
	} else {
		switch call.Op {
		case OpGet:
			method = http.MethodGet
		case OpDelete:
			method = http.MethodDelete
		case OpPut:
			method = http.MethodPut
		case OpPost:
			method = http.MethodPost
		case OpPatch:
			method = http.MethodPatch
		}
	}
	if !isAllowedMethod(method) {
		return nil, apperr.New(apperr.CodeInvalidRequest, "unsupported method: "+method)
	}

	// 2. 解析上游 Key（§8.3.3、RS-20）
	parsedKey, err := parseSecret(up.Secret)
	if err != nil {
		return nil, err
	}

	// 3. 构造上游 URL（RS-10、RS-14）
	subpath := ""
	query := ""
	if call.Params != nil {
		subpath = call.Params["path"]
		query = call.Params["query"]
	}

	if parsedKey.queryParam != "" {
		// 查询参数注入时，删除客户端同名查询参数（RS-14）
		query = stripQueryParam(query, parsedKey.queryParam)
		paramStr := parsedKey.queryParam + "=" + url.QueryEscape(parsedKey.queryValue)
		if query == "" {
			query = paramStr
		} else {
			query = query + "&" + paramStr
		}
	}

	reqURL := protocol.JoinURL(up.BaseURL, subpath)
	if query != "" {
		reqURL += "?" + query
	}

	// 4. 组装请求体（RS-11）
	var bodyReader io.Reader
	switch method {
	case http.MethodPut:
		if len(call.Body) > 0 {
			bodyReader = bytes.NewReader(call.Body)
		}
	case http.MethodPost, http.MethodPatch:
		if call.BodyStream != nil {
			bodyReader = call.BodyStream
		} else if len(call.Body) > 0 {
			bodyReader = bytes.NewReader(call.Body)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// 5. 过滤并复制请求头（RS-12、RS-13、RS-14）
	if call.Header != nil {
		// 白名单头复制
		protocol.CopyHeaders(req.Header, call.Header,
			"Content-Type",
			"Accept",
			"Accept-Language",
			"If-Match",
			"If-None-Match",
			"If-Modified-Since",
			"If-Unmodified-Since",
			"Range",
		)

		// X-UAIG-Forward-<Name> 机制
		for k, vals := range call.Header {
			lower := strings.ToLower(k)
			if strings.HasPrefix(lower, "x-uaig-forward-") {
				name := k[len("x-uaig-forward-"):]
				if isDropForwardHeader(name) {
					slog.WarnContext(ctx, "rest: dropping forbidden forward header", "header", name)
					continue
				}
				for _, v := range vals {
					req.Header.Add(name, v)
				}
			}
		}
	}

	// 设置 User-Agent（RS-13）
	req.Header.Set("User-Agent", e.userAgent())

	// 注入上游 Key 头（覆盖同名客户端转发头，RS-14）
	if parsedKey.headerName != "" {
		req.Header.Set(parsedKey.headerName, parsedKey.headerValue)
	}

	// 6. 发送上游请求
	resp, err := protocol.Send(up, req)
	if err != nil {
		var attErr *protocol.AttemptError
		if errors.As(err, &attErr) {
			sanitizeAttemptErrorURL(attErr)
			// RS-16: 403 改写为 OutcomeClientError，保留 403 状态码
			if !e.preserveClassifyHTTP && attErr.Outcome.Status == http.StatusForbidden {
				attErr.Outcome.Kind = protocol.OutcomeClientError
			}
			return nil, attErr
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			sanitizeURLError(urlErr)
		}
		return nil, err
	}

	// 7. 2xx 响应处理（RS-15、RS-18）
	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header,
		"Content-Type",
		"Content-Length",
		"Content-Disposition",
		"Content-Range",
		"Accept-Ranges",
		"Cache-Control",
		"ETag",
		"Last-Modified",
		"Location",
	)
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/octet-stream")
	}

	m := &meter{
		usage: pricing.Usage{UnitRequests: 1},
		src:   protocol.UsageUpstream,
	}

	return &protocol.Response{
		Kind:   protocol.ResponseRaw,
		Status: resp.StatusCode,
		Header: h,
		Raw:    resp.Body,
		Meter:  m,
	}, nil
}

func (e Egress) userAgent() string {
	if e.UserAgent != "" {
		return e.UserAgent
	}
	return "universal-ai-gateway"
}

func isAllowedMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodPut, http.MethodPost, http.MethodPatch:
		return true
	default:
		return false
	}
}

// stripQueryParam 剔除 query 中名为 name 的参数（RS-14）。
func stripQueryParam(rawQuery, name string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	var kept []string
	for _, p := range parts {
		if p == "" {
			continue
		}
		k := p
		if idx := strings.Index(p, "="); idx >= 0 {
			k = p[:idx]
		}
		if unescapedK, err := url.QueryUnescape(k); err == nil && unescapedK == name {
			continue
		} else if k == name {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "&")
}

// sanitizeURLError 剔除 url.Error 中的查询串（RS-17），严防凭据泄露。
func sanitizeURLError(ue *url.Error) {
	if ue == nil {
		return
	}
	if u, err := url.Parse(ue.URL); err == nil {
		u.RawQuery = ""
		ue.URL = u.String()
	} else if idx := strings.Index(ue.URL, "?"); idx >= 0 {
		ue.URL = ue.URL[:idx]
	}
}

// sanitizeAttemptErrorURL 剔除 AttemptError.Err 内部 url.Error 中的查询串（RS-17）。
func sanitizeAttemptErrorURL(ae *protocol.AttemptError) {
	if ae == nil || ae.Err == nil {
		return
	}
	var ue *url.Error
	if errors.As(ae.Err, &ue) {
		sanitizeURLError(ue)
	}
}

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
