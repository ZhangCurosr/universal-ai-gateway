package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// fakeIngress 只实现 pick 用到的 Protocol 与 Claims；其他方法不会被调用（嵌入的 nil 接口）。
type fakeIngress struct {
	protocol.Ingress
	name   protocol.Protocol
	claims bool
}

func (f fakeIngress) Protocol() protocol.Protocol { return f.name }
func (f fakeIngress) Claims(*http.Request) bool   { return f.claims }

// RTE-02 的每个分支。
func TestPick(t *testing.T) {
	a := fakeIngress{name: "a"}
	b := fakeIngress{name: "b"}
	bClaims := fakeIngress{name: "b", claims: true}
	c := fakeIngress{name: "c"}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	tests := []struct {
		name     string
		ins      []protocol.Ingress
		fallback protocol.Ingress
		want     protocol.Protocol
	}{
		{"single declarer wins without asking", []protocol.Ingress{a}, c, "a"},
		{"first claimer in priority order", []protocol.Ingress{a, bClaims}, c, "b"},
		{"no claimer: fallback if it declared the route", []protocol.Ingress{a, b}, b, "b"},
		{"no claimer and fallback did not declare: highest priority", []protocol.Ingress{b, a}, c, "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pick(tt.ins, tt.fallback, req).Protocol(); got != tt.want {
				t.Errorf("pick() = %s, want %s", got, tt.want)
			}
		})
	}
}

// MW-06：ClientIP 把 Trusted.From 的结果存入 context；可信代理后取 XFF 中最右的不可信地址。
func TestClientIPMiddleware(t *testing.T) {
	trusted, err := clientip.ParseTrusted([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, remote, xff, want string
	}{
		{"direct client ignores xff", "198.51.100.7:1234", "203.0.113.1", "198.51.100.7"},
		{"trusted proxy uses rightmost untrusted hop", "10.0.0.1:443", "203.0.113.1, 203.0.113.9, 10.0.0.2", "203.0.113.9"},
		{"trusted proxy without xff", "10.0.0.1:443", "", "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got netip.Addr
			h := ClientIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = clientip.FromContext(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got.String() != tt.want {
				t.Errorf("client ip = %s, want %s", got, tt.want)
			}
		})
	}
}
