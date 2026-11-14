package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestLANOpen(t *testing.T) {
	for addr, want := range map[string]bool{
		"0.0.0.0:8080": true, ":8080": true, "[::]:8080": true, "192.168.1.5:8080": true,
		"127.0.0.1:8080": false, "[::1]:8080": false, "localhost:8080": false, "bad": false,
	} {
		if got := LANOpen(addr); got != want {
			t.Errorf("LANOpen(%q) = %v", addr, got)
		}
	}
}

func TestLANAddressesSkipsLoopback(t *testing.T) {
	addrs, err := LANAddresses()
	if err != nil {
		t.Skipf("net.Interfaces: %v", err)
	}
	for _, a := range addrs {
		if a.IP == "127.0.0.1" || a.IP == "" || a.Interface == "" {
			t.Errorf("unexpected address %+v", a)
		}
	}
}

// LAN-01 / LAN-02：状态、地址与开关。
func TestShareAPI(t *testing.T) {
	var calls []bool
	writable := true
	env := newTestEnvWith(t, func(d *Deps) {
		d.Config.Server.DataListen = "127.0.0.1:18080"
		d.Share = &ShareDeps{
			Writable: func() bool { return writable },
			SetLANOpen: func(_ context.Context, actor int64, open bool) (int, any) {
				calls = append(calls, open)
				return http.StatusAccepted, map[string]any{"changed": true}
			},
			Interfaces: func() ([]ShareAddress, error) { return []ShareAddress{{IP: "192.168.1.23", Interface: "WLAN"}}, nil },
		}
	})
	rec := env.req("GET", "/admin/api/share", nil, env.adminTok)
	var v ShareView
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	if v.Listen != "127.0.0.1:18080" || v.Port != 18080 || v.LANOpen || !v.ConfigWritable || len(v.Addresses) != 1 || v.Addresses[0].URL != "http://192.168.1.23:18080" || v.URLTemplate != "http://<IP>:18080" || v.OS == "" {
		t.Fatalf("view = %+v", v)
	}
	if rec := env.req("POST", "/admin/api/share", `{"lan_open":true}`, env.adminTok); rec.Code != http.StatusAccepted || len(calls) != 1 || !calls[0] {
		t.Fatalf("POST: %d %v", rec.Code, calls)
	}
	if rec := env.req("POST", "/admin/api/share", `{}`, env.adminTok); rec.Code != 400 {
		t.Fatalf("missing lan_open: %d", rec.Code)
	}
	writable = false
	if rec := env.req("POST", "/admin/api/share", `{"lan_open":false}`, env.adminTok); rec.Code != http.StatusConflict || len(calls) != 1 {
		t.Fatalf("not writable: %d", rec.Code)
	}
	if rec := env.req("GET", "/admin/api/share", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	// 没有注入 Share：只显示状态，开关 409
	plain := newTestEnv(t)
	if rec := plain.req("POST", "/admin/api/share", `{"lan_open":true}`, plain.adminTok); rec.Code != http.StatusConflict {
		t.Fatalf("no share deps: %d", rec.Code)
	}
}

func TestSortAddresses(t *testing.T) {
	a := []ShareAddress{{IP: "100.92.4.86", Interface: "Tailscale"}, {IP: "10.42.100.2", Interface: "RelyVPN"},
		{IP: "172.20.1.2", Interface: "vEthernet"}, {IP: "192.168.3.170", Interface: "WLAN"}}
	SortAddresses(a)
	var got []string
	for _, x := range a {
		got = append(got, x.IP)
	}
	if want := "192.168.3.170,172.20.1.2,10.42.100.2,100.92.4.86"; strings.Join(got, ",") != want {
		t.Fatalf("order = %v", got)
	}
}
