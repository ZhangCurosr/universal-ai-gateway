package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/core"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/files"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
)

// DataDeps 是数据面路由的依赖。
type DataDeps struct {
	Config    *config.Config
	Core      *core.Core
	Registry  *protocol.Registry // 必须已 Seal
	Index     *auth.Index
	Pepper    []byte
	Limiter   *ratelimit.Limiter // 每客户端 RPM（MW-05）
	IPLimiter *ratelimit.Limiter // 鉴权前每 IP RPM（MW-07）；必须与 Limiter 是不同实例
	Overload  *OverloadGuard     // 全局并发与请求体预算（MW-08）
	Trusted   clientip.Trusted   // 可信代理（MW-06）
	// MCP 是 /mcp 端点的处理器（mcp.Server）；nil = 不挂载（config mcp.disabled）。
	// MCPErrorWriter 是该端点中间件使用的错误格式（mcp.WriteHTTPError）。httpapi 不导入 mcp 包，由 cmd/gateway 注入。
	MCP            http.Handler
	MCPErrorWriter ErrorWriter
	// Files 是文件中转的存储（T69）；nil = 没有开启 files，不注册 /files，带 X-UAIG-File 的请求 → 400。
	Files  *files.Store
	Logger *slog.Logger
}

// NewDataRouter 按注册表中全部入站适配器声明的路由组装数据面（spec/09 §1，RTE-01 ~ RTE-04）。
//
// 全局中间件：RequestID → ClientIP → AccessLog → Recover（错误格式用兜底协议）。
// 每个协议端点：IPRateLimit → Overload → Authenticate → RateLimit → 端点处理（错误格式用该协议）。
// GET /healthz 不经过协议中间件。
func NewDataRouter(d DataDeps) http.Handler {
	fallback := d.Registry.Fallback()
	mux := http.NewServeMux()

	type routeKey struct{ method, path string }
	var order []routeKey
	claimants := map[routeKey][]protocol.Ingress{}
	routes := map[routeKey]map[protocol.Protocol]protocol.Route{}
	for _, in := range d.Registry.Ingresses() { // 已按 Priority 排序
		for _, rt := range in.Routes() {
			k := routeKey{rt.Method, rt.Path}
			if _, seen := claimants[k]; !seen {
				order = append(order, k)
				routes[k] = map[protocol.Protocol]protocol.Route{}
			}
			claimants[k] = append(claimants[k], in)
			routes[k][in.Protocol()] = rt
		}
	}

	for _, k := range order {
		handlers := map[protocol.Protocol]http.Handler{}
		for _, in := range claimants[k] {
			rt := routes[k][in.Protocol()]
			handlers[in.Protocol()] = d.protocolChain(in, rt, d.endpoint(in, rt))
		}
		ins := claimants[k]
		mux.Handle(k.method+" "+k.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers[pick(ins, fallback, r).Protocol()].ServeHTTP(w, r)
		}))
	}

	if d.MCP != nil { // RTE-05
		h := chain(d.MCP,
			IPRateLimit(d.IPLimiter, d.Config.Limits.PreAuthIPRPM, d.MCPErrorWriter),
			d.Overload.Middleware(d.MCPErrorWriter, true),
			Authenticate(d.Index, d.Pepper, d.MCPErrorWriter),
			RateLimit(d.Limiter, d.MCPErrorWriter),
		)
		mux.Handle("/mcp", h) // 方法由 mcp.Server 判断（GET / DELETE 返回 405）
	}

	if d.Files != nil { // FS-10 ~ FS-12
		d.mountFiles(mux, fallback.WriteError)
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { // RTE-04
		writeErr(w, r, fallback.WriteError, apperr.New(apperr.CodeNotFound, "no such endpoint"))
	})
	return chain(mux, RequestID, ClientIP(d.Trusted), AccessLog(d.Logger), Recover(d.Logger, fallback.WriteError))
}

// pick 实现 RTE-02：只有一个协议声明该路由时直接用它；否则按优先级询问 Claims，
// 都不认领时交给兜底协议（若它也声明了该路由），再不然交给优先级最高的那个。
func pick(ins []protocol.Ingress, fallback protocol.Ingress, r *http.Request) protocol.Ingress {
	if len(ins) == 1 {
		return ins[0]
	}
	for _, in := range ins {
		if in.Claims(r) {
			return in
		}
	}
	for _, in := range ins {
		if in.Protocol() == fallback.Protocol() {
			return in
		}
	}
	return ins[0]
}

// protocolChain 套用协议端点的中间件（RTE-03），错误格式全部使用 in.WriteError。
func (d DataDeps) protocolChain(in protocol.Ingress, rt protocol.Route, h http.Handler) http.Handler {
	ew := in.WriteError
	return chain(h,
		IPRateLimit(d.IPLimiter, d.Config.Limits.PreAuthIPRPM, ew),
		fileBudget,
		d.Overload.Middleware(ew, rt.Body == protocol.BodyBuffered),
		Authenticate(d.Index, d.Pepper, ew),
		RateLimit(d.Limiter, ew),
		d.fileBody(ew, rt.Body != protocol.BodyNone), // FS-20：鉴权之后、交给内核之前
	)
}

// endpoint 返回端点处理器：RouteCall 交给内核，RouteCatalog 由入站适配器输出目录。
func (d DataDeps) endpoint(in protocol.Ingress, rt protocol.Route) http.Handler {
	if rt.Kind == protocol.RouteCatalog {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.PrincipalFrom(r.Context())
			in.WriteCatalog(w, d.Core.Models(p))
		})
	}
	return d.Core.Handler(in, rt)
}

// chain 按书写顺序套用中间件：chain(h, a, b) 处理顺序为 a → b → h。
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
