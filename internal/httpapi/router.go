package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/proxy"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
)

// DataDeps 是数据面路由的依赖。
type DataDeps struct {
	Config    *config.Config
	Proxy     *proxy.Handler
	Index     *auth.Index
	Pepper    []byte
	Limiter   *ratelimit.Limiter // 每客户端 RPM（MW-05）
	IPLimiter *ratelimit.Limiter // 鉴权前每 IP RPM（MW-07）；必须与 Limiter 是不同实例
	Overload  *OverloadGuard     // 全局并发与请求体预算（MW-08）
	Trusted   clientip.Trusted   // 可信代理（MW-06）
	Logger    *slog.Logger
}

// NewDataRouter 组装数据面路由（spec/09 §1）。
//
// 全局中间件：RequestID → ClientIP → AccessLog → Recover。
// 模型端点与 /v1/models 额外套用：IPRateLimit → Overload → Authenticate → RateLimit。
// /healthz 不套用额外中间件。
func NewDataRouter(d DataDeps) http.Handler {
	mux := http.NewServeMux()
	// TODO(T11): 注册 POST /v1/messages、POST /v1/chat/completions、GET /v1/models（按上面顺序套用中间件）与 GET /healthz。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apperr.WriteHTTP(w, FormatForPath(r.URL.Path), obs.RequestIDFrom(r.Context()), apperr.New(apperr.CodeNotFound, "no such endpoint"))
	})
	return chain(mux, RequestID, ClientIP(d.Trusted), AccessLog(d.Logger), Recover(d.Logger))
}

// chain 按书写顺序套用中间件：chain(h, a, b) 处理顺序为 a → b → h。
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
