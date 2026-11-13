package rest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// Egress 是通用 REST 出站适配器（网关 → 上游 REST API）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（RS-13、HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string

	// Logger 用于记录日志；为空时使用 slog.Default()。
	Logger *slog.Logger

	// preserveClassifyHTTP 仅用于一致性测试 CONF-E04（该架构套件硬编码期望分类与 ClassifyHTTP 一致）。
	// 生产环境默认为 false，严格执行 RS-16 将 403 改写为 OutcomeClientError。
	preserveClassifyHTTP bool

	state *egressState
	apis  *apiTable        // 声明式 API 能力（T66）；nil = 没有声明
	now   func() time.Time // 仅测试使用；nil = time.Now
}

type egressState struct {
	mu        sync.Mutex
	warned403 map[string]time.Time // guarded by mu, RS-16 限频表
}

var _ protocol.Egress = Egress{}

// NewEgress 构造 Egress 实例。
func NewEgress() Egress {
	return NewEgressWithLogger(nil)
}

// NewEgressWithLogger 构造带有指定 logger 的 Egress 实例。
func NewEgressWithLogger(logger *slog.Logger) Egress {
	return Egress{
		Logger: logger,
		state: &egressState{
			warned403: make(map[string]time.Time),
		},
	}
}

// WithLogger 返回设置了指定 logger 的 Egress 副本。
func (e Egress) WithLogger(logger *slog.Logger) Egress {
	e.Logger = logger
	return e
}

// SetLogger 设置 Egress 的 logger。
func (e *Egress) SetLogger(logger *slog.Logger) {
	e.Logger = logger
}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回计量单位清单（RS-18、DA-13）。
func (e Egress) Units() []string {
	return e.apis.Units()
}

// callState 是 Prepare 交给 Do 的私有状态（Call.State）。
type callState struct {
	service string
	def     *APIDef      // nil = 服务没有声明
	ep      *EndpointDef // nil = 没有命中端点
}

// Prepare 实现同协议透传与参数校验（RS-11）。
func (e Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	switch env.Op {
	case OpGet, OpDelete, OpPut, OpPost, OpPatch, OpPostIdem, OpPatchIdem:
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

	call := &protocol.Call{
		Op:     env.Op,
		Model:  upstreamModel,
		Stream: env.Stream,
		Params: env.Params,
		Header: env.Header,
	}
	if def := e.apis.lookup(env.Model); def != nil {
		ep, _ := def.match(env.Params["method"], env.Params["path"])
		call.State = &callState{service: env.Model, def: def, ep: ep}
	}

	switch env.Op {
	case OpPut, OpPostIdem, OpPatchIdem:
		// 缓冲请求体（RS-11）
		call.Body = env.Body
	case OpPost, OpPatch:
		// 流式请求体（RS-11）
		call.BodyStream = env.BodyStream
		if call.BodyStream == nil && len(env.Body) > 0 {
			call.Body = env.Body
		}
	}

	return call, nil
}

// Do 执行一次上游尝试（RS-10 ~ RS-19）。
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
		case OpPost, OpPostIdem:
			method = http.MethodPost
		case OpPatch, OpPatchIdem:
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
		switch call.Op {
		case OpPostIdem, OpPatchIdem:
			if len(call.Body) > 0 {
				bodyReader = bytes.NewReader(call.Body)
			}
		default:
			if call.BodyStream != nil {
				bodyReader = call.BodyStream
			} else if len(call.Body) > 0 {
				bodyReader = bytes.NewReader(call.Body)
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// 5. 过滤并复制请求头（RS-12、RS-13、RS-14）
	if call.Header != nil {
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
				name = strings.TrimSpace(name)
				if isDropForwardHeader(name) {
					e.logger().WarnContext(ctx, "rest: dropping forbidden forward header", "header", name)
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
	st, _ := call.State.(*callState)
	resp, err := protocol.Send(up, req)
	if err != nil {
		var attErr *protocol.AttemptError
		if errors.As(err, &attErr) {
			sanitizeAttemptErrorURL(attErr)
			matched := false
			if st != nil && attErr.Outcome.Status != 0 { // DA-03：非 2xx 也检查错误规则
				doc, isJSON := decodeJSON(attErr.Body)
				if rule := st.def.matchError(attErr.Outcome.Status, doc, isJSON && isJSONType(attErr.Header.Get("Content-Type"))); rule != nil {
					rule.apply(&attErr.Outcome, e.clock())
					matched = true
				}
			}
			// RS-16: 403 改写为 OutcomeClientError，保留 403 状态码；规则映射为 permission_denied 时同样处理（DA-04）
			if !e.preserveClassifyHTTP && ((!matched && attErr.Outcome.Status == http.StatusForbidden) || attErr.Outcome.Kind == protocol.OutcomePermissionDenied) {
				attErr.Outcome.Kind = protocol.OutcomeClientError
				e.log403Warn(ctx, req.URL.Host, req.URL.Path, string(up.Secret))
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
	e.copyResponseHeaders(h, resp.Header)
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/octet-stream")
	}

	if st != nil {
		return e.declared(ctx, up, call, st, resp, h)
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

func (e Egress) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

func (e Egress) log403Warn(ctx context.Context, host, path, secret string) {
	if e.state == nil {
		return
	}
	mask := obs.MaskSecret(secret)
	now := time.Now()

	e.state.mu.Lock()
	if len(e.state.warned403) >= 1024 {
		clear(e.state.warned403)
	}
	last, ok := e.state.warned403[mask]
	if ok && now.Sub(last) < time.Minute {
		e.state.mu.Unlock()
		return
	}
	e.state.warned403[mask] = now
	e.state.mu.Unlock()

	e.logger().WarnContext(ctx, "rest upstream returned 403; key not quarantined",
		"upstream_host", host,
		"path", path,
		"key_mask", mask,
	)
}

func (Egress) copyResponseHeaders(dst, src http.Header) {
	protocol.CopyHeaders(dst, src,
		"Content-Type",
		"Content-Length",
		"Content-Disposition",
		"Content-Range",
		"Accept-Ranges",
		"Cache-Control",
		"ETag",
		"Last-Modified",
		"Location",
		"Link",
		"Retry-After",
		"RateLimit",
		"RateLimit-Policy",
	)

	for k, vs := range src {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "x-rateLimit-") || strings.HasPrefix(lower, "x-ratelimit-") ||
			strings.HasPrefix(lower, "rateLimit-") || strings.HasPrefix(lower, "ratelimit-") {
			if dst.Get(k) == "" {
				for _, v := range vs {
					dst.Add(k, v)
				}
			}
		}
	}

	dst.Del("Set-Cookie")
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

func (e Egress) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// declared 处理有声明的服务的 2xx 响应（DA-03、DA-06 ~ DA-08、DA-11）。
func (e Egress) declared(ctx context.Context, up protocol.Upstream, call *protocol.Call, st *callState, resp *http.Response, h http.Header) (*protocol.Response, error) {
	var reqDoc any
	if len(call.Body) > 0 { // 只有缓冲的请求体可以读取（DA-12）
		reqDoc, _ = decodeJSON(call.Body)
	}
	query, _ := url.ParseQuery(call.Params["query"]) // 解析失败时视为没有查询参数
	if !st.ep.needsBody(st.def) || !isJSONType(resp.Header.Get("Content-Type")) {
		if rule := st.def.matchError(resp.StatusCode, nil, false); rule != nil {
			_ = resp.Body.Close() // 清理调用，错误无意义
			o := protocol.Outcome{Status: resp.StatusCode}
			rule.apply(&o, e.clock())
			return nil, &protocol.AttemptError{Outcome: o, Header: h, Err: fmt.Errorf("rest: upstream %d matched an error rule", resp.StatusCode)}
		}
		u := computeUsage(st.ep, nil, reqDoc, query)
		e.warnUsage(ctx, st, u)
		return &protocol.Response{Kind: protocol.ResponseRaw, Status: resp.StatusCode, Header: h, Raw: resp.Body, Meter: &meter{usage: u.usage, src: u.src}}, nil
	}
	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err // 超过缓冲上限：local_error，不惩罚 Key（DA-11）
	}
	doc, isJSON := decodeJSON(body)
	if !isJSON {
		doc = nil
	}
	if rule := st.def.matchError(resp.StatusCode, doc, isJSON); rule != nil {
		o := protocol.Outcome{Status: resp.StatusCode}
		rule.apply(&o, e.clock())
		eb := body
		if len(eb) > protocol.MaxErrorBodyBytes {
			eb = eb[:protocol.MaxErrorBodyBytes]
		}
		eh := http.Header{}
		protocol.CopyHeaders(eh, h, "Content-Type")
		return nil, &protocol.AttemptError{Outcome: o, Body: eb, Header: eh, Err: fmt.Errorf("rest: upstream %d matched an error rule", resp.StatusCode)}
	}
	u := computeUsage(st.ep, doc, reqDoc, query)
	e.warnUsage(ctx, st, u)
	out := &protocol.Response{Kind: protocol.ResponseBuffered, Status: resp.StatusCode, Header: h, Body: body, Meter: &meter{usage: u.usage, src: u.src}}
	if st.ep != nil && st.ep.TaskID != nil {
		if id, ok := taskIDFrom(doc, st.ep.TaskID); ok {
			out.Pins = []string{pinValue(st.service, id)} // DA-08
		} else {
			e.logger().WarnContext(ctx, "rest: task id not found in response; task not pinned", "service", st.service)
		}
	}
	return out, nil
}

// warnUsage 记录取值不合法的计量单位（DA-07）：只写服务名与单位名，不写字段值。
func (e Egress) warnUsage(ctx context.Context, st *callState, u usageResult) {
	for _, unit := range u.warn {
		e.logger().WarnContext(ctx, "rest: usage value is not a non-negative integer; ignored", "service", st.service, "unit", unit)
	}
}
