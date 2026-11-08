package apperr

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteHTTPAnthropic(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteHTTP(rec, FormatAnthropic, "req_1", New(CodeRateLimited, "slow down").WithRetryAfter(1500*time.Millisecond))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Type != "rate_limit_error" || body.Error.Message != "slow down" || body.RequestID != "req_1" {
		t.Fatalf("body = %+v", body)
	}
}

func TestWriteHTTPOpenAIHidesInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteHTTP(rec, FormatOpenAI, "req_1", errors.New("sql: secret table exploded"))
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret table") {
		t.Fatalf("internal error text leaked: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"internal"`) || !strings.Contains(rec.Body.String(), `"type":"server_error"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWriteHTTPAdmin(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteHTTP(rec, FormatAdmin, "", New(CodeNotFound, "client 42 not found"))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
