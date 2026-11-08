// Package httpapi 实现数据面 HTTP 路由与中间件（spec/09 §1，任务卡 T11）。
//
// 本包不认识任何具体协议：路由来自 protocol.Registry 中各入站适配器声明的 Routes()，
// 错误格式由对应入站适配器的 WriteError 决定。
package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
)

// ErrorWriter 按某个协议的格式写错误响应；通常就是 protocol.Ingress.WriteError。
type ErrorWriter func(w http.ResponseWriter, requestID string, err *apperr.Error)

func writeErr(w http.ResponseWriter, r *http.Request, ew ErrorWriter, err error) {
	ew(w, obs.RequestIDFrom(r.Context()), apperr.Normalize(err))
}

// RequestID 实现 MW-01：总是生成新的请求 ID，忽略客户端传入的同名头。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := obs.NewRequestID()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(obs.WithRequestID(r.Context(), id)))
	})
}

// ClientIP 实现 MW-06：按可信代理解析真实客户端 IP 并存入 context（spec/03 §7）。
func ClientIP(trusted clientip.Trusted) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(clientip.WithIP(r.Context(), trusted.From(r))))
		})
	}
}

// IPRateLimit 实现 MW-07：鉴权之前按来源 IP 限流（spec/09 §5，OVL-04、OVL-05）。rpm <= 0 时直接放行。
func IPRateLimit(l *ratelimit.Limiter, rpm int, ew ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// TODO(T11): 键为 "ip:" + clientip.FromContext(ctx).String()；拒绝时 429 rate_limited + Retry-After。
		return next
	}
}

// OverloadGuard 是全局并发与请求体内存预算（spec/09 §5，OVL-01 ~ OVL-03）。并发安全。
type OverloadGuard struct {
	maxRequests   int64
	maxBodyBytes  int64
	maxSingleBody int64

	inFlight  atomic.Int64
	bodyBytes atomic.Int64
}

// NewOverloadGuard 创建过载保护。maxSingleBody 为单个请求体上限（limits.max_request_body_bytes）。
func NewOverloadGuard(maxRequests int, maxBodyBytes, maxSingleBody int64) *OverloadGuard {
	return &OverloadGuard{maxRequests: int64(maxRequests), maxBodyBytes: maxBodyBytes, maxSingleBody: maxSingleBody}
}

// Middleware 实现 MW-08：占用失败用 ew 写 503 overloaded（Retry-After: 1），请求结束后归还。
// buffered 为 false 时（BodyStream / BodyNone 路由，请求体不进内存）只占并发名额，不占请求体预算（OVL-02）。
func (g *OverloadGuard) Middleware(ew ErrorWriter, buffered bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// TODO(T11): 按 OVL-01 ~ OVL-03 实现；用 CAS 循环占用，禁止「先检查再 Add」。
		return next
	}
}

// InFlight 返回当前占用的请求数与请求体字节数（供测试与管理 API 展示）。
func (g *OverloadGuard) InFlight() (requests, bodyBytes int64) {
	return g.inFlight.Load(), g.bodyBytes.Load()
}

// Recover 实现 MW-03。ew 是兜底协议的错误格式（全局中间件此时还不知道请求属于哪个协议）。
func Recover(log *slog.Logger, ew ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tw := &trackingWriter{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v) // 标准库约定：交给 net/http 中止连接
				}
				log.Error("panic in handler", "request_id", obs.RequestIDFrom(r.Context()),
					"panic", v, "stack", string(debug.Stack()))
				if !tw.wroteHeader {
					writeErr(tw, r, ew, apperr.New(apperr.CodeInternal, "internal error"))
				}
			}()
			next.ServeHTTP(tw, r)
		})
	}
}

// AccessLog 实现 MW-02。
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// TODO(T11): 请求结束后输出一条 Info：request_id, method, path, status, latency_ms, client_id, client_ip。
		return next
	}
}

// Authenticate 实现 MW-04。
func Authenticate(ix *auth.Index, pepper []byte, ew ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// TODO(T11): ExtractCredential → HashToken → Lookup；401 / 403；auth.WithPrincipal。
			writeErr(w, r, ew, apperr.NotImplemented("T11"))
		})
	}
}

// RateLimit 实现 MW-05。必须位于 Authenticate 之后。
func RateLimit(l *ratelimit.Limiter, ew ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// TODO(T11)
		return next
	}
}

// trackingWriter 记录是否已写出响应头。实现 http.Flusher 以支持流式。
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
}

func (t *trackingWriter) WriteHeader(code int) {
	if !t.wroteHeader {
		t.wroteHeader = true
		t.status = code
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	if !t.wroteHeader {
		t.WriteHeader(http.StatusOK)
	}
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 ResponseWriter。
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
