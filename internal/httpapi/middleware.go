// Package httpapi 实现数据面 HTTP 路由与中间件（spec/09 §1，任务卡 T11）。
package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
)

// FormatForPath 按路径决定错误格式：/v1/messages 用 Anthropic，其余用 OpenAI（spec/09 §1.1）。
func FormatForPath(path string) apperr.Format {
	if strings.HasPrefix(path, "/v1/messages") {
		return apperr.FormatAnthropic
	}
	return apperr.FormatOpenAI
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
func IPRateLimit(l *ratelimit.Limiter, rpm int) func(http.Handler) http.Handler {
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

// Middleware 实现 MW-08：占用失败返回 503 overloaded（Retry-After: 1），请求结束后归还。
func (g *OverloadGuard) Middleware(next http.Handler) http.Handler {
	// TODO(T11): 按 OVL-01 ~ OVL-03 实现；用 CAS 循环占用，禁止「先检查再 Add」。
	return next
}

// InFlight 返回当前占用的请求数与请求体字节数（供测试与管理 API 展示）。
func (g *OverloadGuard) InFlight() (requests, bodyBytes int64) {
	return g.inFlight.Load(), g.bodyBytes.Load()
}

// Recover 实现 MW-03。
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
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
					apperr.WriteHTTP(tw, FormatForPath(r.URL.Path), obs.RequestIDFrom(r.Context()),
						apperr.New(apperr.CodeInternal, "internal error"))
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
func Authenticate(ix *auth.Index, pepper []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// TODO(T11): ExtractCredential → HashToken → Lookup；401 / 403；auth.WithPrincipal。
			apperr.WriteHTTP(w, FormatForPath(r.URL.Path), obs.RequestIDFrom(r.Context()), apperr.NotImplemented("T11"))
		})
	}
}

// RateLimit 实现 MW-05。必须位于 Authenticate 之后。
func RateLimit(l *ratelimit.Limiter) func(http.Handler) http.Handler {
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
