package main

import (
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
)

// T71：网页开关局域网共享 → 改配置文件中的 server.data_listen（只改这一行，留 .bak）→ 进程内重启 → 再关闭。
func TestE2E_ShareToggle(t *testing.T) {
	url := restUpstream(t, nil, nil)
	local := freeAddr(t)
	_, port, _ := net.SplitHostPort(local)
	spec := gwSpec{dataListen: local, pools: poolYAML("p1", url), services: svcYAML("s1", "p1", "0")}
	g := startReloadGateway(t, spec)
	before, err := os.ReadFile(g.path)
	if err != nil {
		t.Fatal(err)
	}
	share := func() map[string]any {
		var v map[string]any
		doJSON(t, "GET", g.adminURL()+"/share", g.adminToken, nil, &v)
		return v
	}
	if v := share(); v["lan_open"] != false || v["config_writable"] != true || v["listen"] != local {
		t.Fatalf("initial view = %+v", v)
	}

	var out map[string]any
	code := doJSON(t, "POST", g.adminURL()+"/share", g.adminToken, map[string]any{"lan_open": true}, &out)
	if code == http.StatusUnprocessableEntity && runtime.GOOS != "windows" {
		// 一些系统不允许在同一端口上先绑定 0.0.0.0 再关闭 127.0.0.1：预检失败，配置文件必须恢复原样
		if after, _ := os.ReadFile(g.path); string(after) != string(before) {
			t.Fatal("config file not restored after a failed switch")
		}
		t.Skipf("same-port rebind not supported on %s: %+v", runtime.GOOS, out)
	}
	if code != http.StatusAccepted || out["changed"] != true || out["listen"] != "0.0.0.0:"+port {
		t.Fatalf("turn on = %d %+v", code, out)
	}
	rec := g.waitRecord(out["reload"].(map[string]any)["record_id"].(float64))
	if rec["status"] != reloadStatusApplied {
		t.Fatalf("record = %+v", rec)
	}
	after, _ := os.ReadFile(g.path)
	bak, _ := os.ReadFile(g.path + ".bak")
	if string(after) != strings.Replace(string(before), `"`+local+`"`, `"0.0.0.0:`+port+`"`, 1) || string(bak) != string(before) {
		t.Fatalf("config file = %q\nbackup = %q", after, bak)
	}
	if v := share(); v["lan_open"] != true {
		t.Fatalf("after turn on = %+v", v)
	}
	token := g.createClient("u")
	g.addKey("p1", "k1")
	if code, _ := g.get(token, "/rest/s1/x"); code != 200 {
		t.Fatal("data plane does not serve after turning on")
	}
	// 再次开启：没有变化，不写文件
	out = nil
	if code := doJSON(t, "POST", g.adminURL()+"/share", g.adminToken, map[string]any{"lan_open": true}, &out); code != 200 || out["changed"] != false {
		t.Fatalf("no-op = %d %+v", code, out)
	}
	// 关闭
	out = nil
	if code := doJSON(t, "POST", g.adminURL()+"/share", g.adminToken, map[string]any{"lan_open": false}, &out); code != http.StatusAccepted {
		t.Fatalf("turn off = %d %+v", code, out)
	}
	g.waitRecord(out["reload"].(map[string]any)["record_id"].(float64))
	if v := share(); v["lan_open"] != false || v["listen"] != "127.0.0.1:"+port {
		t.Fatalf("after turn off = %+v", v)
	}
	if !strings.Contains(g.stderr.String(), "config edited by admin") {
		t.Error("edit not logged (LAN-04)")
	}
}
