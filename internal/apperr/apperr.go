// Package apperr 定义跨包传播的业务错误与错误码（spec/09 §2.1）。
//
// 使用方式：
//
//	return apperr.New(apperr.CodeInsufficientQuota, "client quota exhausted")
//	return apperr.Wrap(apperr.CodeUpstreamError, "read upstream body", err)
//
// Message 会返回给客户端，禁止包含凭据、SQL、内部路径；Cause 只进日志。
package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Code 是稳定的机器可读错误码，会出现在 API 响应中，不能随意改名。
type Code string

const (
	CodeInvalidRequest      Code = "invalid_request"
	CodeUnauthenticated     Code = "unauthenticated"
	CodeInsufficientQuota   Code = "insufficient_quota"
	CodePermissionDenied    Code = "permission_denied"
	CodeNotFound            Code = "not_found"
	CodeConflict            Code = "conflict"
	CodeRequestTooLarge     Code = "request_too_large"
	CodeRateLimited         Code = "rate_limited"
	CodeInternal            Code = "internal"
	CodeNotImplemented      Code = "not_implemented"
	CodeUpstreamError       Code = "upstream_error"
	CodeUpstreamUnavailable Code = "upstream_unavailable"
	CodeOverloaded          Code = "overloaded"
	CodeTimeout             Code = "timeout"
)

var httpStatus = map[Code]int{
	CodeInvalidRequest:      http.StatusBadRequest,
	CodeUnauthenticated:     http.StatusUnauthorized,
	CodeInsufficientQuota:   http.StatusPaymentRequired,
	CodePermissionDenied:    http.StatusForbidden,
	CodeNotFound:            http.StatusNotFound,
	CodeConflict:            http.StatusConflict,
	CodeRequestTooLarge:     http.StatusRequestEntityTooLarge,
	CodeRateLimited:         http.StatusTooManyRequests,
	CodeInternal:            http.StatusInternalServerError,
	CodeNotImplemented:      http.StatusNotImplemented,
	CodeUpstreamError:       http.StatusBadGateway,
	CodeUpstreamUnavailable: http.StatusServiceUnavailable,
	CodeOverloaded:          http.StatusServiceUnavailable,
	CodeTimeout:             http.StatusGatewayTimeout,
}

// HTTPStatus 返回错误码对应的 HTTP 状态码；未知错误码返回 500。
func (c Code) HTTPStatus() int {
	if s, ok := httpStatus[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Error 是业务错误。零值不可用，请用 New / Wrap 构造。
type Error struct {
	Code       Code
	Message    string        // 返回给客户端的消息
	RetryAfter time.Duration // > 0 时写 Retry-After 头
	Cause      error         // 内部原因，只进日志
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// New 构造一个不带内部原因的业务错误。
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Wrap 构造一个带内部原因的业务错误。
func Wrap(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, Cause: cause}
}

// WithRetryAfter 返回设置了 RetryAfter 的副本。
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	cp := *e
	cp.RetryAfter = d
	return &cp
}

// NotImplemented 用于骨架中尚未实现的方法，task 为任务卡编号（如 "T05"）。
func NotImplemented(task string) *Error {
	return New(CodeNotImplemented, "not implemented yet: TODO("+task+")")
}

// As 从错误链中取出 *Error。
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf 返回错误链中第一个 *Error 的错误码；err 为 nil 返回 ""；非业务错误返回 CodeInternal。
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	if e, ok := As(err); ok {
		return e.Code
	}
	return CodeInternal
}
