package rest

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestParseSecret_Valid(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantHeader  string
		wantVal     string
		wantQuery   string
		wantQVal    string
	}{
		{
			name:       "bearer explicit",
			raw:        "bearer:my-secret-token",
			wantHeader: "Authorization",
			wantVal:    "Bearer my-secret-token",
		},
		{
			name:       "default to bearer",
			raw:        "sk-universal-default-key",
			wantHeader: "Authorization",
			wantVal:    "Bearer sk-universal-default-key",
		},
		{
			name:       "header custom",
			raw:        "header:X-Custom-Key:value-1234",
			wantHeader: "X-Custom-Key",
			wantVal:    "value-1234",
		},
		{
			name:       "header authorization custom prefix",
			raw:        "header:Authorization:Token custom-token",
			wantHeader: "Authorization",
			wantVal:    "Token custom-token",
		},
		{
			name:       "header x-api-key",
			raw:        "header:X-Api-Key:secret-api-key",
			wantHeader: "X-Api-Key",
			wantVal:    "secret-api-key",
		},
		{
			name:      "query param",
			raw:       "query:api_key:my_secret_val",
			wantQuery: "api_key",
			wantQVal:  "my_secret_val",
		},
		{
			name:      "query param with dot and dash",
			raw:       "query:api-key.v1:val@123",
			wantQuery: "api-key.v1",
			wantQVal:  "val@123",
		},
		{
			name:       "basic auth standard",
			raw:        "basic:alice:secret123",
			wantHeader: "Authorization",
			wantVal:    "Basic YWxpY2U6c2VjcmV0MTIz",
		},
		{
			name:       "basic auth password with colons",
			raw:        "basic:alice:pass:with:colons",
			wantHeader: "Authorization",
			wantVal:    "Basic YWxpY2U6cGFzczp3aXRoOmNvbG9ucw==",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseSecret([]byte(tc.raw))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantHeader != "" && (p.headerName != tc.wantHeader || p.headerValue != tc.wantVal) {
				t.Errorf("header = %s: %s, want %s: %s", p.headerName, p.headerValue, tc.wantHeader, tc.wantVal)
			}
			if tc.wantQuery != "" && (p.queryParam != tc.wantQuery || p.queryValue != tc.wantQVal) {
				t.Errorf("query = %s: %s, want %s: %s", p.queryParam, p.queryValue, tc.wantQuery, tc.wantQVal)
			}
		})
	}
}

func TestParseSecret_Invalid(t *testing.T) {
	forbiddenSecrets := []string{
		"header:no-colon-separator",
		"header::value-only",
		"header:Host:evil.com",
		"header:Cookie:session=123",
		"header:Proxy-Authorization:Basic xxx",
		"header:Content-Length:100",
		"header:Transfer-Encoding:chunked",
		"header:Connection:close",
		"header:Upgrade:websocket",
		"header:TE:trailers",
		"header:Trailer:x",
		"header:Keep-Alive:timeout=5",
		"header:X-Goog-Api-Key:key",
		"header:X-Forwarded-For:1.2.3.4",
		"header:X-Forwarded-Host:example.com",
		"header:X-UAIG-Test:val",
		"header:bad token:val",
		"header:good:\r\ninjection",
		"bearer:token\nwith-newline",
		"query:no-colon-separator",
		"query::value-only",
		"query:bad$name:123",
		"query:bad@name:123",
		"query:bad space:123",
		"basic:no-colon-separator",
		"basic:user\n:pass",
		"basic:user:pass\r",
	}

	for _, s := range forbiddenSecrets {
		t.Run(s, func(t *testing.T) {
			p, err := parseSecret([]byte(s))
			if err == nil {
				t.Fatalf("expected error for %q, got parsedSecret: %+v", s, p)
			}
			var attErr *protocol.AttemptError
			if !errors.As(err, &attErr) {
				t.Fatalf("expected *protocol.AttemptError, got %T: %v", err, err)
			}
			if attErr.Outcome.Kind != protocol.OutcomeAuthFailed {
				t.Errorf("expected OutcomeAuthFailed, got %v", attErr.Outcome.Kind)
			}
			if attErr.Outcome.Status != http.StatusUnauthorized {
				t.Errorf("expected Status 401, got %d", attErr.Outcome.Status)
			}
			if attErr.Err.Error() != "rest upstream key format is invalid" {
				t.Errorf("expected message 'rest upstream key format is invalid', got %q", attErr.Err.Error())
			}

			// 严防凭据明文泄露（RS-20，AGENTS.md §3 第 1 条）
			if strings.Contains(attErr.Err.Error(), s) {
				t.Errorf("error contains raw secret %q", s)
			}
		})
	}
}
