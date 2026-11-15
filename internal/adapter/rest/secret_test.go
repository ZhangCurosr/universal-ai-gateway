package rest

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestValidateSecretPrefix(t *testing.T) {
	for _, prefix := range []string{"", "bearer:", "header:Authorization:Token ", "header:X-Api-Key:", "query:api_key:", "basic:alice:", "basic::"} {
		if err := ValidateSecretPrefix(prefix); err != nil {
			t.Fatalf("ValidateSecretPrefix(%q): %v", prefix, err)
		}
	}
	for _, prefix := range []string{"unknown:", "bearer:extra", "query:api_key:extra", "basic:alice:extra", "header:no-colon", "header:Host:", "header:X-Forwarded-For:", "query:bad name:", "header:Authorization:Token \r\n", "basic:a\n:", "header:Authorization:\x00", "header:Authorization:\x7f"} {
		err := ValidateSecretPrefix(prefix)
		if err == nil || err.Error() != "rest secret_prefix is invalid" {
			t.Fatalf("invalid prefix accepted or error included its value: %v", err)
		}
		if _, err := newAPITable([]APIDef{{Service: "svc", SecretPrefix: prefix}}); err == nil {
			t.Fatal("API table accepted an invalid prefix")
		}
	}
}

func TestParseSecretWithPrefix(t *testing.T) {
	for _, tc := range []struct{ name, prefix, raw, want string }{
		{"default", "", "raw-key", "Bearer raw-key"},
		{"raw custom scheme", "header:Authorization:Token ", "raw-key", "Token raw-key"},
		{"explicit bearer wins", "header:Authorization:Token ", "bearer:explicit", "Bearer explicit"},
		{"explicit header wins", "header:Authorization:Token ", "header:Authorization:Custom explicit", "Custom explicit"},
		{"explicit query wins", "header:Authorization:Token ", "query:key:explicit", "explicit"},
		{"explicit basic wins", "header:Authorization:Token ", "basic:a:b", "Basic YTpi"},
		{"pool query", "query:key:", "raw-key", "raw-key"},
		{"pool basic", "basic:a:", "b", "Basic YTpi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			before := bytes.Clone(raw)
			p, err := parseSecretWithPrefix(raw, tc.prefix)
			if err != nil {
				t.Fatal(err)
			}
			got := p.headerValue
			if p.queryParam != "" {
				got = p.queryValue
			}
			if got != tc.want || !bytes.Equal(raw, before) {
				t.Fatal("prefix was not applied correctly or mutated the original Key")
			}
		})
	}
	for _, tc := range []struct{ prefix, raw string }{
		{"unknown:", "raw-key"},
		{"header:Authorization:Token ", "bad\r\nkey"},
		{"header:Authorization:Token ", "header:Host:invalid"},
	} {
		if _, err := parseSecretWithPrefix([]byte(tc.raw), tc.prefix); err == nil {
			t.Fatal("invalid prefix or credential was accepted")
		}
	}
}

func TestParseSecret_Valid(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantHeader string
		wantVal    string
		wantQuery  string
		wantQVal   string
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
