// Package core 是网关内核：一次调用从准入到结算的完整编排（spec/13，任务卡 T10）。
//
// 内核是协议无关的：它只通过 protocol 包的接口与适配器交互，只从 protocol.Envelope 了解请求，
// 永远不读取请求体中的具体字段，也不导入任何 internal/adapter/* 包（ARCH-07）。
// 内核也不假设上游是大模型：计费按适配器报告的计量单位，响应可以是缓冲体、事件流或字节流。
// 新增协议不需要修改本包。
//
// 进入本包之前，httpapi 已完成：请求 ID、客户端 IP、过载保护、鉴权（Principal 在 context 中）、限流。
// 本包禁止访问数据库（ARCH-01）。
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// Route 是一个客户端服务名（config models[].name）解析后的路由信息。
type Route struct {
	Model    config.ModelConfig
	Pool     config.PoolConfig
	Upstream protocol.Protocol // 池的上游协议
	Egress   protocol.Egress
	Price    pricing.Price // 已解析的价目表
	Reserve  pricing.Usage // models[].reserve
}

// Deps 是内核的依赖。全部字段必填（Now 除外）。
type Deps struct {
	Config     *config.Config
	Registry   *protocol.Registry // 必须已 Seal
	Ledger     ledger.Ledger
	Scheduler  *scheduler.Scheduler
	Audit      *audit.Pipeline
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time // 为 nil 时用 time.Now
}

// Core 处理调用请求。并发安全。
type Core struct {
	d          Deps
	routes     map[string]Route        // 客户端服务名 → 路由，构造后只读
	poolRoutes map[string]Route        // 池名 → 该池中名称最小的服务的路由（CORE-11），构造后只读
	auditHook  func(store.AuditRecord) // 仅测试使用：观察提交的审计记录
}

// New 创建内核并解析全部路由：
//   - CORE-01：每个池的 protocol 必须有已注册的 Egress；
//   - CORE-05：每个模型的价目表必须为该 Egress.Units() 中的每个单位定价（可以是 "0"）。
//
// 任一不满足返回错误，进程拒绝启动。
func New(d Deps) (*Core, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	c := &Core{d: d, routes: map[string]Route{}, poolRoutes: map[string]Route{}}
	for _, m := range d.Config.Models {
		p, ok := d.Config.Pool(m.Pool)
		if !ok {
			return nil, fmt.Errorf("core: model %q references unknown pool %q", m.Name, m.Pool)
		}
		up := protocol.Protocol(p.Protocol)
		eg, ok := d.Registry.Egress(up)
		if !ok {
			return nil, fmt.Errorf("core: pool %q uses protocol %q but no egress adapter is registered for it", p.Name, p.Protocol)
		}
		price := pricing.Price{}
		for unit, s := range m.Price {
			r, err := pricing.ParseRate(s)
			if err != nil {
				return nil, fmt.Errorf("core: model %q: price.%s: %w", m.Name, unit, err)
			}
			price[unit] = r
		}
		for _, unit := range eg.Units() {
			if _, ok := price[unit]; !ok {
				return nil, fmt.Errorf("core: model %q: protocol %q reports unit %q but price.%s is not configured (use \"0\" for free)",
					m.Name, up, unit, unit)
			}
		}
		c.routes[m.Name] = Route{
			Model:    m,
			Pool:     *p,
			Upstream: up,
			Egress:   eg,
			Price:    price,
			Reserve:  pricing.Usage(m.Reserve),
		}
		if prev, ok := c.poolRoutes[p.Name]; !ok || m.Name < prev.Model.Name {
			c.poolRoutes[p.Name] = c.routes[m.Name]
		}
	}
	return c, nil
}

// resolveRoute 实现 LIFE-07b 与 CORE-11（已实现）：
//   - env.Model 非空：查路由；不存在 → 400 invalid_request；不在白名单 → 403 permission_denied；
//   - env.Model 为空（只允许严格亲和，IN-G03）：用固定记录所在的池确定路由；未命中 → 404 not_found。
//     白名单不再检查：固定记录按 clientID 隔离，只有创建者本人能命中。
func (c *Core) resolveRoute(env *protocol.Envelope, p *auth.Principal, affinityKey string) (Route, error) {
	if env.Model != "" {
		r, ok := c.routes[env.Model]
		if !ok {
			return Route{}, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("unknown model %q", env.Model))
		}
		if p != nil && !p.ModelAllowed(env.Model) {
			return Route{}, apperr.New(apperr.CodePermissionDenied, fmt.Sprintf("model %q is not allowed for this client", env.Model))
		}
		return r, nil
	}
	if !env.Affinity.Strict || affinityKey == "" {
		return Route{}, apperr.New(apperr.CodeInvalidRequest, "model: required")
	}
	pool, ok := c.d.Scheduler.PinnedPool(affinityKey)
	if !ok {
		return Route{}, apperr.New(apperr.CodeNotFound, "unknown or expired resource; it may have been created before a gateway restart")
	}
	r, ok := c.poolRoutes[pool]
	if !ok {
		return Route{}, apperr.New(apperr.CodeInternal, "pinned key belongs to a pool without models")
	}
	return r, nil
}

// Route 返回服务路由。
func (c *Core) Route(model string) (Route, bool) {
	r, ok := c.routes[model]
	return r, ok
}

// Models 返回 p 被允许使用的服务，按 ID 升序（供 RouteCatalog 端点使用）。
func (c *Core) Models(p *auth.Principal) []protocol.ModelInfo {
	out := make([]protocol.ModelInfo, 0, len(c.routes))
	for name, r := range c.routes {
		if p == nil || p.ModelAllowed(name) {
			out = append(out, protocol.ModelInfo{ID: name, Protocol: r.Upstream})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Handler 返回处理入站适配器 in 的调用端点 rt（rt.Kind == RouteCall）的 http.Handler。
// 调用前请求体尚未读取；Principal 与请求 ID 已在 context 中。
//
// 处理流程见 spec/13 §2：读请求体（按 rt.Body，上限 bodyLimit）→ in.Parse → 服务路由与白名单 →
// 生成 Call（同协议：Egress.Prepare；跨协议：Registry.Translator，没有则 400）→ 预留（reserveUsage）→
// 亲和键 → 尝试循环（Egress.Do）→ 跨协议时 Translator.Response → 按 Response.Kind 转发 →
// 记录 Pins → defer 收尾（Finish、Settle/Release、审计）。
// 所有错误响应都用 in.WriteError（首字节前）或 in.StreamError（事件流首字节后）。
func (c *Core) Handler(in protocol.Ingress, rt protocol.Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := &exchange{c: c, in: in, rt: rt, r: r, w: &trackingWriter{ResponseWriter: w}}
		x.serve()
		if x.abort {
			panic(http.ErrAbortHandler) // 字节流中途失败：中止连接，让客户端看到不完整的响应（STR-02）。收尾已在 serve 中完成
		}
	})
}

// exchange 是一次请求的处理状态。只在处理该请求的 goroutine 中使用。
type exchange struct {
	c  *Core
	in protocol.Ingress
	rt protocol.Route
	r  *http.Request
	w  *trackingWriter

	requestID string
	principal *auth.Principal
	start     time.Time
	ttfb      time.Duration // 开始转发上游响应的时刻（相对 start）；-1 = 没有转发上游响应
	audit     store.AuditRecord
	errCode   apperr.Code
	abort     bool

	env         *protocol.Envelope
	route       Route
	reservation *ledger.Reservation
	reserved    pricing.Usage
}

// serve 实现 spec/13 §2 的全部步骤；收尾动作全部在 finish 中（一个 defer）。
func (x *exchange) serve() {
	x.start = x.c.d.Now()
	x.ttfb = -1
	x.requestID = obs.RequestIDFrom(x.r.Context())
	x.audit = store.AuditRecord{RequestID: x.requestID, Op: x.rt.Op, IngressProtocol: string(x.in.Protocol()),
		TTFBMillis: -1, CreatedAt: x.start.UTC()}
	defer x.finish()

	p, ok := auth.PrincipalFrom(x.r.Context())
	if !ok {
		x.fail(apperr.New(apperr.CodeInternal, "request reached the core without authentication"))
		return
	}
	x.principal = p
	x.audit.ClientID = p.ClientID

	// 1. 读取请求体（LIFE-03、CORE-06）
	limit := bodyLimit(x.rt, x.c.d.Config.Limits)
	var body []byte
	switch x.rt.Body {
	case protocol.BodyBuffered:
		b, err := io.ReadAll(http.MaxBytesReader(x.w, x.r.Body, limit))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				x.fail(apperr.New(apperr.CodeRequestTooLarge, fmt.Sprintf("request body exceeds %d bytes", limit)))
			} else {
				x.fail(apperr.New(apperr.CodeInvalidRequest, "failed to read request body"))
			}
			return
		}
		body = b
	case protocol.BodyStream:
		if x.r.ContentLength > limit {
			x.fail(apperr.New(apperr.CodeRequestTooLarge, fmt.Sprintf("request body exceeds %d bytes", limit)))
			return
		}
	}

	// 2. 解析（LIFE-07a）
	env, err := x.in.Parse(x.r, x.rt, body)
	if err != nil {
		x.fail(err)
		return
	}
	x.env = env
	x.audit.Model, x.audit.Stream = env.Model, env.Stream
	var upload *watchedBody
	if x.rt.Body == protocol.BodyStream {
		upload = &watchedBody{r: http.MaxBytesReader(x.w, x.r.Body, limit)}
		env.BodyStream = upload
	}

	// 3. 亲和键与路由（LIFE-10、LIFE-07b、CORE-11）
	affinityKey := AffinityKey(p.ClientID, env.Header, env.Affinity)
	route, err := x.c.resolveRoute(env, p, affinityKey)
	if err != nil {
		x.fail(err)
		return
	}
	x.route = route
	x.audit.Model, x.audit.UpstreamProtocol = route.Model.Name, string(route.Upstream)

	// 4. 生成上游调用（CORE-03）
	call, tr, err := x.prepareCall()
	if err != nil {
		x.fail(err)
		return
	}

	// 5. 预留（LIFE-09、LED-01）
	x.reserved = reserveUsage(env, route)
	amount, unpriced := pricing.Cost(route.Price, x.reserved)
	x.logUnpriced(unpriced)
	res, err := x.c.d.Ledger.Reserve(p.ClientID, x.requestID, amount)
	if err != nil {
		x.fail(err)
		return
	}
	x.reservation = res

	// 6. 尝试循环（spec/07 §6）与转发
	bodyBytes := int64(len(body))
	if x.rt.Body == protocol.BodyStream {
		bodyBytes = max(x.r.ContentLength, 0)
	}
	x.attempts(call, tr, affinityKey, bodyBytes, upload)
}

// prepareCall 实现 spec/13 §3：同协议透传，否则查找转换器，找不到返回 400（CORE-03）。
func (x *exchange) prepareCall() (*protocol.Call, protocol.Translator, error) {
	if x.env.Protocol == x.route.Upstream {
		call, err := x.route.Egress.Prepare(x.env, x.route.Model.UpstreamModel)
		return call, nil, err
	}
	tr, ok := x.c.d.Registry.Translator(x.env.Protocol, x.route.Upstream)
	if !ok {
		return nil, nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf(
			"model %q is served by protocol %q; translation from %q is not supported",
			x.route.Model.Name, x.route.Upstream, x.env.Protocol))
	}
	call, err := tr.Request(x.env, x.route.Model.UpstreamModel)
	return call, tr, err
}

// attempts 实现 spec/07 §6 与 spec/13 §4、§5。
func (x *exchange) attempts(call *protocol.Call, tr protocol.Translator, affinityKey string, bodyBytes int64, upload *watchedBody) {
	pool := x.route.Pool
	maxAttempts := pool.MaxAttempts
	if call.BodyStream != nil || x.env.Affinity.Strict { // CORE-07、RTY-06
		maxAttempts = 1
	}
	doTimeout := headerTimeout(pool.Timeouts, bodyBytes)
	if !x.env.Stream { // 缓冲响应在 Do 内读完整个响应体：给足整体时长（spec/13 §5）
		doTimeout = max(doTimeout, pool.Timeouts.NonStreamTotal)
	}
	var exclude []int64
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if x.r.Context().Err() != nil { // RTY-05
			x.audit.Outcome = protocol.OutcomeCanceled.String()
			return
		}
		lease, err := x.c.d.Scheduler.Acquire(scheduler.AcquireRequest{
			Pool: pool.Name, AffinityKey: affinityKey, Strict: x.env.Affinity.Strict, Exclude: exclude})
		if err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		x.audit.KeyID = lease.Key.ID
		x.audit.Attempts++

		done, err := x.attempt(lease, call, tr, doTimeout, upload)
		if done {
			return
		}
		lastErr = err
		var ae *protocol.AttemptError
		if !errors.As(err, &ae) || !ae.Outcome.Kind.Retryable() {
			break
		}
		exclude = append(exclude, lease.Key.ID)
	}
	if upload != nil && upload.tooLarge.Load() {
		lastErr = apperr.New(apperr.CodeRequestTooLarge, "request body exceeds the upload limit")
	}
	x.failAttempts(lastErr)
}

// attempt 执行一次尝试。done 为 true 表示响应已经转发（成功或首字节后失败），不再重试；
// 否则 err 是本次失败的原因（已 Finish 租约）。
func (x *exchange) attempt(lease *scheduler.Lease, call *protocol.Call, tr protocol.Translator,
	doTimeout time.Duration, upload *watchedBody) (done bool, err error) {
	ctx, cancel := context.WithCancel(x.r.Context())
	defer cancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(doTimeout, func() { timedOut.Store(true); cancel() })
	resp, err := x.route.Egress.Do(ctx, protocol.Upstream{
		BaseURL: x.route.Pool.BaseURL, Secret: lease.Key.Secret, Client: x.c.d.HTTPClient,
		MaxBufferedBytes: maxBufferedResponseBytes}, call)
	timer.Stop()
	if err != nil {
		o := x.attemptOutcome(err, &timedOut)
		lease.Finish(o)
		x.audit.Outcome = o.Kind.String()
		return false, err
	}
	defer closeResponse(resp)

	idle := newIdleTimer(x.route.Pool.Timeouts.StreamIdle, cancel)
	defer idle.stop()

	// 事件流：先读第一个事件再写响应头，第一个事件之前的失败仍可重试（spec/13 §4.2 第 1 步）
	var first sse.Event
	hasFirst := false
	if resp.Kind == protocol.ResponseEvents {
		ev, nerr := resp.Events.Next()
		switch {
		case nerr == nil:
			first, hasFirst = ev, true
		case errors.Is(nerr, io.EOF):
		default:
			ae := &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeNetworkError}, Err: nerr}
			if !errors.As(nerr, &ae) && idle.fired.Load() {
				ae.Outcome.Kind = protocol.OutcomeTimeout
			}
			o := x.attemptOutcome(ae, &timedOut)
			lease.Finish(o)
			x.audit.Outcome = o.Kind.String()
			return false, ae
		}
	}

	out := resp
	if tr != nil { // CORE-04：用量、Fault、Pins 仍取原始 resp
		if hasFirst { // 已预读的第一个上游事件要交还给转换器
			resp.Events = &replayStream{first: first, inner: resp.Events}
		}
		converted, terr := tr.Response(x.env, resp)
		if terr != nil {
			lease.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess}) // 上游本身没有问题
			x.audit.Outcome = protocol.OutcomeSuccess.String()
			x.settle(resp, lease.Key.ID)
			x.fail(apperr.Wrap(apperr.CodeUpstreamError, "could not translate the upstream response", terr))
			return true, nil
		}
		out = converted
		defer closeResponse(out)
		hasFirst = false
		if out.Kind == protocol.ResponseEvents {
			ev, nerr := out.Events.Next()
			switch {
			case nerr == nil:
				first, hasFirst = ev, true
			case !errors.Is(nerr, io.EOF):
				x.ttfb = x.c.d.Now().Sub(x.start)
				writeResponseHeader(x.w, out, x.requestID, false)
				x.completeAttempt(lease, resp, x.streamFailure(sse.NewWriter(x.w), nerr, idle))
				return true, nil
			}
		}
	}

	x.ttfb = x.c.d.Now().Sub(x.start)
	var result streamResult
	switch out.Kind {
	case protocol.ResponseBuffered:
		result = x.forwardBuffered(out)
	case protocol.ResponseEvents:
		result = x.forwardEvents(out, first, hasFirst, idle)
	case protocol.ResponseRaw:
		result = x.forwardRaw(out, idle)
	default:
		result = streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeLocalError}}
		x.fail(apperr.New(apperr.CodeInternal, "adapter returned an unknown response kind"))
	}
	if timedOut.Load() && result.outcome.Kind != protocol.OutcomeSuccess {
		result.outcome.Kind = protocol.OutcomeTimeout
	}
	x.completeAttempt(lease, resp, result)
	return true, nil
}

// completeAttempt 在响应转发后：记录 Pins（CORE-08）、结束租约、结算。
func (x *exchange) completeAttempt(lease *scheduler.Lease, resp *protocol.Response, result streamResult) {
	if result.outcome.Kind == protocol.OutcomeSuccess {
		for _, pin := range resp.Pins {
			key := AffinityKey(x.principal.ClientID, nil, protocol.AffinityHint{Source: protocol.AffinityPin, Value: []byte(pin)})
			lease.Pin(key, x.route.Pool.PinTTL)
		}
	}
	lease.Finish(result.outcome)
	x.audit.Outcome = result.outcome.Kind.String()
	if result.errCode != "" {
		x.errCode = result.errCode
	}
	x.abort = result.abort
	x.settle(resp, lease.Key.ID)
}

// attemptOutcome 实现 CORE-09、CORE-10：取出 Outcome，并按客户端断开 / 内核超时修正分类。
func (x *exchange) attemptOutcome(err error, timedOut *atomic.Bool) protocol.Outcome {
	o := protocol.Outcome{Kind: protocol.OutcomeLocalError}
	var ae *protocol.AttemptError
	if errors.As(err, &ae) {
		o = ae.Outcome
	}
	switch {
	case x.r.Context().Err() != nil:
		o.Kind = protocol.OutcomeCanceled
	case timedOut.Load() && ae != nil:
		o.Kind = protocol.OutcomeTimeout
	}
	return o
}

// failAttempts 实现 spec/07 §6.1：全部尝试失败时返回给客户端的错误；预留由 finish 释放。
func (x *exchange) failAttempts(err error) {
	if x.r.Context().Err() != nil {
		return // 客户端已断开，不写响应
	}
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) {
		if e, ok := apperr.As(err); ok {
			x.fail(e)
		} else {
			x.c.d.Logger.Error("core: adapter returned a non-protocol error", "request_id", x.requestID, "error", err)
			x.fail(apperr.New(apperr.CodeInternal, "internal error"))
		}
		return
	}
	o := ae.Outcome
	switch o.Kind {
	case protocol.OutcomeRateLimited:
		x.fail(apperr.New(apperr.CodeRateLimited, "upstream rate limited").WithRetryAfter(o.RetryAfter))
	case protocol.OutcomeClientError:
		if x.env.Protocol == x.route.Upstream && len(ae.Body) > 0 { // 同协议透传：原样转发上游错误
			for k, vs := range ae.Header {
				x.w.Header()[k] = vs
			}
			x.w.Header().Set("X-Request-Id", x.requestID)
			x.w.WriteHeader(o.Status)
			_, _ = x.w.Write(ae.Body) // 写客户端失败无法补救
			x.errCode = apperr.CodeInvalidRequest
			return
		}
		x.fail(apperr.New(apperr.CodeInvalidRequest, "upstream rejected the request"))
	case protocol.OutcomeOverloaded:
		x.fail(apperr.New(apperr.CodeUpstreamUnavailable, "upstream is overloaded").WithRetryAfter(o.RetryAfter))
	case protocol.OutcomeTimeout:
		x.fail(apperr.New(apperr.CodeTimeout, "upstream timed out"))
	case protocol.OutcomeCanceled:
		// 客户端已断开
	default:
		x.fail(apperr.New(apperr.CodeUpstreamError, "upstream request failed"))
	}
}

// fail 在首字节之前用入站协议的格式写错误（CORE-02）；之后只记录错误码。
func (x *exchange) fail(err error) {
	e := apperr.Normalize(err)
	x.errCode = e.Code
	if e.Code == apperr.CodeInternal {
		x.c.d.Logger.Error("core: internal error", "request_id", x.requestID, "error", err)
	}
	if x.w.wroteHeader() {
		return
	}
	x.w.Header().Set("X-Request-Id", x.requestID)
	x.in.WriteError(x.w, x.requestID, e)
}

// settle 实现 spec/13 §2.1：按 Meter 结算一次。
func (x *exchange) settle(resp *protocol.Response, keyID int64) {
	if x.reservation == nil {
		return
	}
	res := x.reservation
	x.reservation = nil
	u, src := resp.Meter.Usage()
	settled, source := settledUsage(x.reserved, u, src)
	cost, unpriced := pricing.Cost(x.route.Price, settled)
	x.logUnpriced(unpriced)
	ev := store.UsageEvent{RequestID: x.requestID, ClientID: x.principal.ClientID, KeyID: keyID, Model: x.route.Model.Name,
		Op: x.env.Op, Usage: settled, CostNanoUSD: cost, UsageSource: source, CreatedAt: x.c.d.Now().UTC()}
	if err := x.c.d.Ledger.Settle(res, ev); err != nil {
		x.c.d.Logger.Error("core: settle failed", "request_id", x.requestID, "error", err)
	}
}

func (x *exchange) logUnpriced(units []string) {
	if len(units) > 0 { // CORE-05：适配器报告了未声明的单位
		x.c.d.Logger.Error("core: usage has unpriced units; charged as 0", "request_id", x.requestID,
			"service", x.route.Model.Name, "units", units)
	}
}

// finish 是唯一的收尾 defer（LIFE-14 ~ LIFE-16）：释放未结算的预留、提交审计记录。
func (x *exchange) finish() {
	if x.reservation != nil { // 所有尝试都没有得到可转发的响应（LED-09）
		x.c.d.Ledger.Release(x.reservation)
		x.reservation = nil
	}
	x.audit.Status = x.w.status
	if x.audit.Status == 0 && x.r.Context().Err() != nil {
		x.audit.Status = statusClientClosed
	}
	x.audit.ErrorCode = string(x.errCode)
	if x.ttfb >= 0 {
		x.audit.TTFBMillis = x.ttfb.Milliseconds()
	}
	x.audit.LatencyMillis = x.c.d.Now().Sub(x.start).Milliseconds()
	x.c.submitAudit(x.audit)
}

// statusClientClosed 是客户端在收到任何字节前断开时审计记录的状态码（spec/13 §2.2）。
const statusClientClosed = 499

func (c *Core) submitAudit(rec store.AuditRecord) {
	if c.d.Audit != nil {
		c.d.Audit.Submit(rec)
	}
	if c.auditHook != nil {
		c.auditHook(rec)
	}
}

// watchedBody 包装流式请求体，记录是否因超过上限而失败（用于把上传超限报告为 413）。
type watchedBody struct {
	r        io.Reader
	tooLarge atomic.Bool
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		b.tooLarge.Store(true)
	}
	return n, err
}

// replayStream 先产出已预读的 first，再继续读 inner（转译路径把预读的事件交还给转换器）。
type replayStream struct {
	first sse.Event
	used  bool
	inner protocol.EventStream
}

func (s *replayStream) Next() (sse.Event, error) {
	if !s.used {
		s.used = true
		return s.first, nil
	}
	return s.inner.Next()
}

func (s *replayStream) Close() error { return s.inner.Close() }

// bodyLimit 实现 CORE-06：缓冲请求体取 min(rt.MaxBodyBytes, limits.max_request_body_bytes)，
// 流式请求体取 min(rt.MaxBodyBytes, limits.max_stream_body_bytes)；rt.MaxBodyBytes == 0 表示不额外限制。
// BodyNone 返回 0。
func bodyLimit(rt protocol.Route, l config.LimitsConfig) int64 {
	var global int64
	switch rt.Body {
	case protocol.BodyBuffered:
		global = l.MaxRequestBodyBytes
	case protocol.BodyStream:
		global = l.MaxStreamBodyBytes
	default:
		return 0
	}
	if rt.MaxBodyBytes > 0 && rt.MaxBodyBytes < global {
		return rt.MaxBodyBytes
	}
	return global
}

// reserveUsage 实现 LED-01 的用量部分：pricing.ReserveUsage(env.Estimate, route.Reserve)。
func reserveUsage(env *protocol.Envelope, route Route) pricing.Usage {
	return pricing.ReserveUsage(env.Estimate, route.Reserve)
}

// settledUsage 实现 spec/06 §3.3：决定结算用量与 usage_source。
//   - UsageUpstream：用上游用量，"upstream"；
//   - UsageEstimated：适配器给出的单位用它的值，其余单位用预留用量补齐，"estimated"；
//   - UsageNone：用预留用量，"estimated"。
func settledUsage(reserved, u pricing.Usage, src protocol.UsageSource) (pricing.Usage, string) {
	switch src {
	case protocol.UsageUpstream:
		return u.Clean(), store.UsageSourceUpstream
	case protocol.UsageEstimated:
		out := pricing.Usage{}
		for k, v := range reserved {
			out[k] = v
		}
		for k, v := range u {
			out[k] = v
		}
		return out.Clean(), store.UsageSourceEstimated
	default:
		return reserved.Clean(), store.UsageSourceEstimated
	}
}

// headerTimeout 实现 spec/13 §5：response_header_base + response_header_per_mib × ceil(bodyBytes / 1 MiB)。
func headerTimeout(t config.TimeoutsConfig, bodyBytes int64) time.Duration {
	const mib = 1 << 20
	n := (max(bodyBytes, 0) + mib - 1) / mib
	return t.ResponseHeaderBase + time.Duration(n)*t.ResponseHeaderPerMiB
}
