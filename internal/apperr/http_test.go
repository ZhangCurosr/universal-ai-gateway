package apperr

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, New(CodeRateLimited, "slow down").WithRetryAfter(1500*time.Millisecond), map[string]string{"m": "slow down"})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d retry-after=%q ct=%q", rec.Code, rec.Header().Get("Retry-After"), rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `"m":"slow down"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestNormalizeHidesInternal(t *testing.T) {
	e := Normalize(errors.New("sql: secret table exploded"))
	if e.Code != CodeInternal || strings.Contains(e.Message, "secret") {
		t.Fatalf("Normalize() = %+v", e)
	}
	orig := New(CodeNotFound, "x")
	if Normalize(orig) != orig {
		t.Fatal("Normalize() replaced an existing *Error")
	}
}
