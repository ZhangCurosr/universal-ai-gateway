package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"syscall"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestClassifyHTTP(t *testing.T) {
	tests := []struct {
		status int
		want   OutcomeKind
	}{
		{200, OutcomeSuccess},
		{400, OutcomeClientError},
		{404, OutcomeClientError},
		{413, OutcomeClientError},
		{422, OutcomeClientError},
		{401, OutcomeAuthFailed},
		{403, OutcomePermissionDenied},
		{429, OutcomeRateLimited},
		{408, OutcomeTimeout},
		{503, OutcomeOverloaded},
		{529, OutcomeOverloaded},
		{500, OutcomeServerError},
		{502, OutcomeServerError},
		{504, OutcomeServerError},
	}
	for _, tt := range tests {
		if got := ClassifyHTTP(tt.status, http.Header{}, now).Kind; got != tt.want {
			t.Errorf("ClassifyHTTP(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestRetryable(t *testing.T) {
	for _, k := range []OutcomeKind{OutcomeClientError, OutcomeCanceled, OutcomeLocalError, OutcomeSuccess} {
		if k.Retryable() {
			t.Errorf("%v.Retryable() = true, want false", k)
		}
	}
	for _, k := range []OutcomeKind{OutcomeRateLimited, OutcomeAuthFailed, OutcomeOverloaded, OutcomeServerError, OutcomeTimeout, OutcomeNetworkError} {
		if !k.Retryable() {
			t.Errorf("%v.Retryable() = false, want true", k)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		h    map[string]string
		want time.Duration
	}{
		{"seconds", map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{"http-date", map[string]string{"Retry-After": now.Add(90 * time.Second).Format(http.TimeFormat)}, 90 * time.Second},
		{"anthropic reset", map[string]string{"anthropic-ratelimit-requests-reset": now.Add(time.Minute).Format(time.RFC3339)}, time.Minute},
		{"openai reset", map[string]string{"x-ratelimit-reset-tokens": "6m0s"}, 6 * time.Minute},
		{"max of several", map[string]string{"Retry-After": "5", "x-ratelimit-reset-requests": "20s"}, 20 * time.Second},
		{"past date ignored", map[string]string{"Retry-After": now.Add(-time.Minute).Format(http.TimeFormat)}, 0},
		{"none", map[string]string{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.h {
				h.Set(k, v)
			}
			if got := ParseRetryAfter(h, now); got != tt.want {
				t.Fatalf("ParseRetryAfter() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyNetError(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	tests := []struct {
		name string
		err  error
		ctx  context.Context
		want OutcomeKind
	}{
		{"client canceled", errors.New("any"), canceled, OutcomeCanceled},
		{"deadline", fmt.Errorf("do: %w", context.DeadlineExceeded), live, OutcomeTimeout},
		{"gateway cancel", fmt.Errorf("do: %w", context.Canceled), live, OutcomeTimeout},
		{"WSAENOBUFS", fmt.Errorf("dial: %w", syscall.Errno(10055)), live, OutcomeLocalError},
		{"other", errors.New("connection reset"), live, OutcomeNetworkError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyNetError(tt.err, tt.ctx).Kind; got != tt.want {
				t.Fatalf("ClassifyNetError() = %v, want %v", got, tt.want)
			}
		})
	}
}
