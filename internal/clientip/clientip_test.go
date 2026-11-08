package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestFrom(t *testing.T) {
	trusted, err := ParseTrusted([]string{"127.0.0.1/32", "10.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"CIP-01 untrusted remote ignores XFF", "203.0.113.9:5000", []string{"1.1.1.1"}, "203.0.113.9"},
		{"CIP-02 trusted proxy single hop", "127.0.0.1:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"CIP-02 rightmost untrusted wins (spoofed left entry)", "127.0.0.1:5000", []string{"6.6.6.6, 198.51.100.7, 10.1.2.3"}, "198.51.100.7"},
		{"CIP-02 multiple headers", "127.0.0.1:5000", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"CIP-03 all trusted", "127.0.0.1:5000", []string{"10.0.0.5, 10.0.0.6"}, "10.0.0.5"},
		{"CIP-03 garbage XFF", "127.0.0.1:5000", []string{"not-an-ip"}, "127.0.0.1"},
		{"no XFF", "127.0.0.1:5000", nil, "127.0.0.1"},
		{"CIP-04 mapped IPv6", "[::ffff:127.0.0.1]:5000", []string{"::ffff:198.51.100.7"}, "198.51.100.7"},
		{"IPv6 loopback proxy", "[::1]:5000", []string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := trusted.From(r).String(); got != tt.want {
				t.Fatalf("From() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestZeroValueTrustsNothing(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	if got := (Trusted{}).From(r).String(); got != "127.0.0.1" {
		t.Fatalf("zero Trusted From() = %s, want 127.0.0.1", got)
	}
}

func TestParseTrustedRejectsBareIP(t *testing.T) {
	if _, err := ParseTrusted([]string{"10.0.0.1"}); err == nil {
		t.Fatal("ParseTrusted(bare IP) error = nil")
	}
}
