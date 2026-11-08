package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestHTTPStatus(t *testing.T) {
	tests := []struct {
		code Code
		want int
	}{
		{CodeInvalidRequest, http.StatusBadRequest},
		{CodeInsufficientQuota, http.StatusPaymentRequired},
		{CodeOverloaded, http.StatusServiceUnavailable},
		{Code("bogus"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		if got := tt.code.HTTPStatus(); got != tt.want {
			t.Errorf("%q.HTTPStatus() = %d, want %d", tt.code, got, tt.want)
		}
	}
}

func TestCodeOfThroughWrapping(t *testing.T) {
	base := New(CodeRateLimited, "slow down")
	wrapped := fmt.Errorf("outer: %w", base)
	if got := CodeOf(wrapped); got != CodeRateLimited {
		t.Fatalf("CodeOf(wrapped) = %q, want %q", got, CodeRateLimited)
	}
	if got := CodeOf(errors.New("plain")); got != CodeInternal {
		t.Fatalf("CodeOf(plain) = %q, want %q", got, CodeInternal)
	}
	if got := CodeOf(nil); got != "" {
		t.Fatalf("CodeOf(nil) = %q, want empty", got)
	}
}

func TestWrapUnwrap(t *testing.T) {
	cause := errors.New("disk full")
	err := Wrap(CodeInternal, "write failed", cause)
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is(Wrap(...), cause) = false, want true")
	}
}
