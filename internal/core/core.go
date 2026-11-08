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
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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
	routes     map[string]Route // 客户端服务名 → 路由，构造后只读
	poolRoutes map[string]Route // 池名 → 该池中名称最小的服务的路由（CORE-11），构造后只读
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
		// TODO(T10): 按 spec/13 §2 实现；收尾动作全部放在一个 defer 中。
		in.WriteError(w, obs.RequestIDFrom(r.Context()), apperr.NotImplemented("T10"))
	})
}

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
