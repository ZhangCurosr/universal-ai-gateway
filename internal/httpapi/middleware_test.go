package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
)

func TestRequestIDIgnoresClientValue(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = obs.RequestIDFrom(r.Context()) }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "client-chosen")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got := rec.Header().Get("X-Request-Id")
	if got == "client-chosen" || !strings.HasPrefix(got, "req_") || len(got) != 28 || seen != got {
		t.Fatalf("X-Request-Id = %q, ctx = %q", got, seen)
	}
}

func TestRecoverReturns500(t *testing.T) {
	h := Recover(obs.Discard())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"api_error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFormatForPath(t *testing.T) {
	if FormatForPath("/v1/messages") != apperr.FormatAnthropic || FormatForPath("/v1/chat/completions") != apperr.FormatOpenAI {
		t.Fatal("FormatForPath mismatch")
	}
}
