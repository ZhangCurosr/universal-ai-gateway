// Package clientip 按可信代理列表解析真实客户端 IP（spec/03 §7，CIP-01 ~ CIP-05）。
package clientip

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Trusted 是可信反向代理的 CIDR 集合。零值表示不信任任何代理。并发安全（只读）。
type Trusted struct {
	prefixes []netip.Prefix
}

// ParseTrusted 解析 CIDR 列表（CFG-11 已在配置校验中保证格式正确）。
func ParseTrusted(cidrs []string) (Trusted, error) {
	t := Trusted{}
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return Trusted{}, fmt.Errorf("trusted proxy %q: %w", c, err)
		}
		t.prefixes = append(t.prefixes, p.Masked())
	}
	return t, nil
}

// Contains 报告 ip 是否属于可信代理。
func (t Trusted) Contains(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range t.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// From 返回请求的真实客户端 IP；RemoteAddr 无法解析时返回零值 netip.Addr{}。
func (t Trusted) From(r *http.Request) netip.Addr {
	remote := parseRemote(r.RemoteAddr)
	if !remote.IsValid() || !t.Contains(remote) {
		return remote // CIP-01
	}
	var hops []netip.Addr
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(h, ",") {
			if ip, err := netip.ParseAddr(strings.TrimSpace(part)); err == nil {
				hops = append(hops, ip.Unmap()) // CIP-04
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- { // CIP-02：从右往左
		if !t.Contains(hops[i]) {
			return hops[i]
		}
	}
	if len(hops) > 0 { // CIP-03
		return hops[0]
	}
	return remote
}

func parseRemote(addr string) netip.Addr {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

type ctxKey struct{}

// WithIP 把客户端 IP 存入 context（由 httpapi.ClientIP 中间件调用）。
func WithIP(ctx context.Context, ip netip.Addr) context.Context {
	return context.WithValue(ctx, ctxKey{}, ip)
}

// FromContext 取出客户端 IP；不存在返回零值。
func FromContext(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(ctxKey{}).(netip.Addr)
	return ip
}
