package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/proxy"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
)

// DataDeps 是数据面路由的依赖。
type DataDeps struct {
	Config  *config.Config
	Proxy   *proxy.Handler
	Index   *auth.Index
	Pepper  []byte
	Limiter *ratelimit.Limiter
	Logger  *slog.Logger
}

// NewDataRouter 组装数据面路由（spec/09 §1）。
func NewDataRouter(d DataDeps) http.Handler {
	mux := http.NewServeMux()
	// TODO(T11): 注册 POST /v1/messages、POST /v1/chat/completions、GET /v1/models（需鉴权+限流），GET /healthz（无需鉴权）。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apperr.WriteHTTP(w, FormatForPath(r.URL.Path), obs.RequestIDFrom(r.Context()), apperr.New(apperr.CodeNotFound, "no such endpoint"))
	})
	return chain(mux, RequestID, AccessLog(d.Logger), Recover(d.Logger))
}

// chain 按书写顺序套用中间件：chain(h, a, b) 处理顺序为 a → b → h。
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
