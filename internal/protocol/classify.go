package protocol

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"
)

// ClassifyHTTP 把上游非 2xx 响应映射为 Outcome（spec/07 §5、spec/08 §7）。
// 2xx 返回 OutcomeSuccess。
func ClassifyHTTP(status int, h http.Header, now time.Time) Outcome {
	o := Outcome{Status: status}
	switch {
	case status >= 200 && status < 300:
		o.Kind = OutcomeSuccess
	case status == http.StatusUnauthorized:
		o.Kind = OutcomeAuthFailed
	case status == http.StatusForbidden:
		o.Kind = OutcomePermissionDenied
	case status == http.StatusTooManyRequests:
		o.Kind = OutcomeRateLimited
		o.RetryAfter = ParseRetryAfter(h, now)
	case status == http.StatusRequestTimeout:
		o.Kind = OutcomeTimeout
	case status == 529 || status == http.StatusServiceUnavailable:
		o.Kind = OutcomeOverloaded
		o.RetryAfter = ParseRetryAfter(h, now)
	case status >= 500:
		o.Kind = OutcomeServerError
	default: // 其余 4xx（400/404/413/422……）
		o.Kind = OutcomeClientError
	}
	return o
}

// Windows Winsock 错误码。
const (
	wsaEADDRINUSE = 10048
	wsaENOBUFS    = 10055
)

// ClassifyNetError 把发送上游请求时的 Go 错误映射为 Outcome。
// clientCtx 是客户端请求的 context，用于区分「客户端断开」与「网关自己的超时」。
func ClassifyNetError(err error, clientCtx context.Context) Outcome {
	switch {
	case err == nil:
		return Outcome{Kind: OutcomeSuccess}
	case clientCtx != nil && clientCtx.Err() != nil:
		return Outcome{Kind: OutcomeCanceled}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return Outcome{Kind: OutcomeTimeout}
	case errors.Is(err, context.Canceled):
		// 网关自己取消（例如响应头超时定时器触发）
		return Outcome{Kind: OutcomeTimeout}
	case isLocalResourceError(err):
		return Outcome{Kind: OutcomeLocalError}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Outcome{Kind: OutcomeTimeout}
	}
	return Outcome{Kind: OutcomeNetworkError}
}

// isLocalResourceError 识别本机资源耗尽（端口、文件句柄），这些错误不应惩罚上游 Key。
func isLocalResourceError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch uintptr(errno) {
	case wsaENOBUFS, wsaEADDRINUSE:
		return true
	}
	return errors.Is(errno, syscall.EMFILE) || errors.Is(errno, syscall.ENFILE) ||
		errors.Is(errno, syscall.EADDRINUSE) || errors.Is(errno, syscall.ENOBUFS)
}

// ParseRetryAfter 解析上游建议的等待时长，取各来源中最大的正值；都没有返回 0（spec/08 §7）。
func ParseRetryAfter(h http.Header, now time.Time) time.Duration {
	var best time.Duration
	consider := func(d time.Duration) {
		if d > best {
			best = d
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			consider(time.Duration(secs) * time.Second)
		} else if t, err := http.ParseTime(v); err == nil {
			consider(t.Sub(now))
		}
	}
	for _, name := range []string{"anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset"} {
		if v := h.Get(name); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				consider(t.Sub(now))
			}
		}
	}
	for _, name := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if v := h.Get(name); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				consider(d)
			}
		}
	}
	return best
}
