package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// CLI-12：表头、BOM、引号中的逗号与换行、制表符分隔、列数不一致、引号错误
func TestCSVRows(t *testing.T) {
	data := "\xef\xbb\xbfemail , api_key,status\n" +
		"\"a,b@x\",k1,ok\n" +
		"\"multi\nline\",k2,ok\n" +
		"only,two\n" +
		"\n" +
		"c@x,\"k3\"\"q\",banned\n" +
		"d@x,\"bad\"quote,ok\n" +
		"e@x,k5,ok\n"
	rows, err := csvRows([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		line int
		obj  map[string]string
		why  string
	}
	var g []got
	for _, r := range rows {
		x := got{line: r.line, why: r.why}
		if r.text != "" {
			if err := json.Unmarshal([]byte(r.text), &x.obj); err != nil {
				t.Fatal(err)
			}
		}
		g = append(g, x)
	}
	if len(g) != 6 {
		t.Fatalf("rows = %+v", g)
	}
	if g[0].line != 2 || g[0].obj["email"] != "a,b@x" || g[0].obj["api_key"] != "k1" {
		t.Errorf("row 1 = %+v", g[0])
	}
	if g[1].line != 3 || g[1].obj["email"] != "multi\nline" {
		t.Errorf("row 2 = %+v", g[1])
	}
	if g[2].line != 5 || !strings.Contains(g[2].why, "2 columns") {
		t.Errorf("short row = %+v", g[2])
	}
	if g[3].obj["api_key"] != `k3"q` || g[3].obj["status"] != "banned" {
		t.Errorf("escaped quote row = %+v", g[3])
	}
	if g[4].why != "malformed CSV row" || g[4].line != 8 {
		t.Errorf("malformed row = %+v", g[4])
	}
	if g[5].obj["api_key"] != "k5" {
		t.Errorf("row after a malformed one = %+v", g[5])
	}

	rows, err = csvRows([]byte("api_key\tstatus\nt1\tok\n"))
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].text, `"api_key":"t1"`) {
		t.Fatalf("tsv = %+v %v", rows, err)
	}
	if rows, err := csvRows(nil); err != nil || rows != nil {
		t.Fatalf("empty = %+v %v", rows, err)
	}
}

func syncKeys(t *testing.T, g *restGateway, pool string) map[string]syncKey {
	t.Helper()
	var out struct {
		Keys []syncKey `json:"keys"`
	}
	doJSON(t, "GET", "http://"+g.admin+"/admin/api/keys", g.adminToken, nil, &out)
	m := map[string]syncKey{}
	for _, k := range out.Keys {
		if k.Pool == pool {
			m[k.Name] = k
		}
	}
	return m
}

// CLI-12 ~ CLI-14、CLI-16：CSV 同步的新增、更新、停用、恢复、安全阈值；不碰其他标签与手工操作
func TestKeysSync(t *testing.T) {
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "sy", Protocol: "rest", BaseURL: "http://127.0.0.1:1"})
		c.Models = append(c.Models, config.ModelConfig{Name: "sy-svc", Pool: "sy", Price: map[string]string{"requests": "0"}})
		c.Normalize()
	})
	env := func(k string) string {
		return map[string]string{"UAIG_ADMIN_URL": "http://" + g.admin, "UAIG_ADMIN_TOKEN": g.adminToken}[k]
	}
	file := filepath.Join(t.TempDir(), "accounts.csv")
	secrets := []string{"sk-sync-AAAA", "sk-sync-BBBB", "sk-sync-CCCC", "sk-sync-DDDD", "alice@x.example", "pw-secret-1"}
	write := func(rows ...string) {
		t.Helper()
		if err := os.WriteFile(file, []byte("email,password,api_key,status,expires_at\n"+strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	A, B, C, D := "alice@x.example,pw-secret-1,sk-sync-AAAA,ok,", "b@x,pw,sk-sync-BBBB,ok,", "c@x,pw,sk-sync-CCCC,ok,2030-01-01 00:00:00", "d@x,pw,sk-sync-DDDD,ok,"
	run := func(args ...string) (int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		base := []string{"keys", "sync", "--pool", "sy", "--file", file, "--format", "csv", "--where", "status=ok",
			"--expires-field", "expires_at", "--tz", "+08:00", "--label", "src", "--name-prefix", "sy"}
		code := realMainIO(append(base, args...), strings.NewReader(""), &out, &errb, env)
		for _, x := range append(secrets, g.adminToken) {
			if strings.Contains(out.String()+errb.String(), x) {
				t.Errorf("output leaks a secret:\n%s%s", out.String(), errb.String())
			}
		}
		return code, out.String() + errb.String()
	}

	write(A, B, C)
	if code, out := run(); code != 0 || !strings.Contains(out, "synced: imported 3, updated 0, disabled 0, restored 0") {
		t.Fatalf("first sync: %d %s", code, out)
	}
	// 手工添加一把无标签的 Key，并由管理员停用一把同标签的 Key：同步都不能动它们
	doJSON(t, "POST", "http://"+g.admin+"/admin/api/keys", g.adminToken, map[string]any{"pool": "sy", "name": "manual", "secret": "sk-manual-1"}, nil)
	doJSON(t, "POST", "http://"+g.admin+"/admin/api/keys", g.adminToken, map[string]any{"pool": "sy", "name": "adm", "secret": "sk-adm-1", "label": "src"}, nil)
	ks := syncKeys(t, g, "sy")
	doJSON(t, "PATCH", "http://"+g.admin+"/admin/api/keys/"+jsonInt(ks["adm"].ID), g.adminToken, map[string]any{"state": "disabled"}, nil)
	if ks["sy-001"].Label != "src" || ks["sy-003"].persisted() != "ready" {
		t.Fatalf("after first sync = %+v", ks)
	}

	// B 被封、C 从文件中删除、新增 D：停用 B、C（2/5，未超过一半），新增 D，更新 A
	write(A, strings.Replace(B, ",ok,", ",banned,", 1), D)
	if code, out := run(); code != 0 || !strings.Contains(out, "synced: imported 1, updated 1, disabled 2, restored 0") {
		t.Fatalf("second sync: %d %s", code, out)
	}
	ks = syncKeys(t, g, "sy")
	if ks["sy-002"].State != "disabled" || ks["sy-002"].StateReason != syncDisabledReason || ks["sy-003"].State != "disabled" {
		t.Fatalf("B and C must be disabled by sync: %+v", ks)
	}
	if ks["manual"].State != "ready" || ks["adm"].StateReason != "disabled by admin" {
		t.Fatalf("sync touched keys it does not own: %+v", ks)
	}

	// B 恢复为 ok：只恢复 B；管理员停用的 adm 不会因此恢复
	write(A, B, D)
	if code, out := run(); code != 0 || !strings.Contains(out, "disabled 0, restored 1") {
		t.Fatalf("restore sync: %d %s", code, out)
	}
	if ks = syncKeys(t, g, "sy"); ks["sy-002"].State != "ready" || ks["sy-002"].StateReason != "" || ks["sy-003"].State != "disabled" {
		t.Fatalf("after restore = %+v", ks)
	}

	// 文件只剩被封的行：要停用 A、B、D（3/5 > 一半）→ 拒绝；--force 后执行
	write(strings.Replace(A, ",ok,", ",banned,", 1))
	if code, out := run(); code != 1 || !strings.Contains(out, "refusing to disable 3 of 5") || !strings.Contains(out, "disabled 0") {
		t.Fatalf("safety check: %d %s", code, out)
	}
	if ks = syncKeys(t, g, "sy"); ks["sy-001"].State != "ready" {
		t.Fatal("refused sync must not disable anything")
	}
	if code, out := run("--force"); code != 0 || !strings.Contains(out, "disabled 3") {
		t.Fatalf("forced sync: %d %s", code, out)
	}
	if code, out := run("--dry-run"); code != 0 || !strings.Contains(out, "would sync: 0 keys in the source") {
		t.Fatalf("dry-run: %d %s", code, out)
	}
	if ks = syncKeys(t, g, "sy"); ks["manual"].State != "ready" {
		t.Fatal("forced sync touched the unlabeled key")
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n) // 整数必然可以序列化
	return string(b)
}

// 参数校验（CLI-13 ~ CLI-15）
func TestKeysSyncFlags(t *testing.T) {
	env := func(k string) string { return map[string]string{"UAIG_ADMIN_TOKEN": "t"}[k] }
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"sync", "--pool", "p", "--file", "f"}, "needs --label"},
		{[]string{"sync", "--pool", "p", "--file", "f", "--label", "l", "--watch", "30s"}, "at least 1m"},
		{[]string{"import", "--pool", "p", "--file", "f", "--force"}, "only for keys sync"},
		{[]string{"import", "--pool", "p", "--file", "f", "--format", "xml"}, "jsonl, lines or csv"},
		{[]string{"import", "--pool", "p", "--file", "f", "--format", "lines", "--where", "a=b"}, "need --format jsonl or csv"},
		{[]string{"export"}, "usage: gateway keys import|sync"},
	} {
		var out, errb bytes.Buffer
		if code := realMainIO(append([]string{"keys"}, tt.args...), strings.NewReader(""), &out, &errb, env); code != 2 || !strings.Contains(errb.String(), tt.want) {
			t.Errorf("%v: %d %q, want %q", tt.args, code, errb.String(), tt.want)
		}
	}
}

// CLI-15：--watch 只在文件变化后同步；收到中断后退出
func TestKeysSyncWatch(t *testing.T) {
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "sw", Protocol: "rest", BaseURL: "http://127.0.0.1:1"})
		c.Models = append(c.Models, config.ModelConfig{Name: "sw-svc", Pool: "sw", Price: map[string]string{"requests": "0"}})
		c.Normalize()
	})
	file := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(file, []byte("sk-watch-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &importOpts{sync: true, pool: "sw", file: file, format: "lines", label: "w", namePrefix: "sw", update: true,
		watch: 20 * time.Millisecond, conds: map[string]string{}, loc: time.UTC, base: "http://" + g.admin, token: g.adminToken}
	var mu sync.Mutex
	var out, errb bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- runSync(ctx, o, lockedWriter{&mu, &out}, lockedWriter{&mu, &errb}) }()
	lines := func() int { mu.Lock(); defer mu.Unlock(); return strings.Count(out.String(), "synced:") }
	deadline := time.Now().Add(5 * time.Second)
	for lines() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // 文件没有变化：不再同步
	if n := lines(); n != 1 {
		t.Fatalf("syncs without a change = %d", n)
	}
	if err := os.WriteFile(file, []byte("sk-watch-1\nsk-watch-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for lines() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 || lines() != 2 || !strings.Contains(out.String(), "imported 1, updated 1") {
		t.Fatalf("watch: code %d out %q err %q", code, out.String(), errb.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestKeyNameSeq(t *testing.T) {
	for _, tt := range []struct {
		name string
		n    int
		ok   bool
	}{{"p-001", 1, true}, {"p-1056", 1056, true}, {"p-", 0, false}, {"p-1a", 0, false}, {"q-001", 0, false}, {"p-x-1", 0, false}} {
		if n, ok := keyNameSeq(tt.name, "p"); n != tt.n || ok != tt.ok {
			t.Errorf("keyNameSeq(%q) = %d %v", tt.name, n, ok)
		}
	}
}
