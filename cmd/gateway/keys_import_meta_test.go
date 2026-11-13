package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// T62：导入时读取过期时间与标签，默认跳过已过期账号，--update 更新已存在的 Key
func TestKeysImportMetadata(t *testing.T) {
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "imp", Protocol: "rest", BaseURL: "http://127.0.0.1:1"})
		c.Models = append(c.Models, config.ModelConfig{Name: "imp-svc", Pool: "imp", Price: map[string]string{"requests": "0"}})
		c.Normalize()
	})
	dir := t.TempDir()
	future := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	secrets := []string{"mk_SECRET_A1", "mk_SECRET_B2", "mk_SECRET_C3", "mk_SECRET_D4", "mk_SECRET_E5", "carol@example.com"}
	write := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	file := write("accounts.jsonl",
		`{"api_key":"mk_SECRET_A1","expires_at":"`+future.Format(time.RFC3339)+`","batch":"b1"}`,
		`{"api_key":"mk_SECRET_B2","expires_at":`+strconv.FormatInt(future.Unix(), 10)+`,"batch":"b2"}`,
		`{"api_key":"mk_SECRET_C3","expires_at":"`+past.Format("2006-01-02 15:04:05")+`","batch":"b1"}`,
		`{"api_key":"mk_SECRET_D4","expires_at":"next week","email":"carol@example.com"}`,
		`{"api_key":"mk_SECRET_E5","batch":"b3"}`,
	)
	env := func(k string) string {
		return map[string]string{"UAIG_ADMIN_URL": "http://" + g.admin, "UAIG_ADMIN_TOKEN": g.adminToken}[k]
	}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := realMainIO(append([]string{"keys", "import", "--pool", "imp"}, args...), strings.NewReader(""), &out, &errb, env)
		for _, x := range append(secrets, g.adminToken, "next week") {
			if strings.Contains(out.String()+errb.String(), x) {
				t.Errorf("output leaks %q", x)
			}
		}
		return code, out.String(), errb.String()
	}

	code, out, errOut := run("--file", file, "--expires-field", "expires_at", "--tz", "UTC", "--label-field", "batch")
	if code != 0 || !strings.Contains(out, "imported 3, duplicate 0, skipped 1 (filtered or invalid), failed 0, updated 0, expired 1") {
		t.Fatalf("import: %d %q %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "line 3: skipped (expired)") || !strings.Contains(errOut, "line 4: skipped (bad expires_at)") {
		t.Fatalf("stderr = %q", errOut)
	}
	keys := listKeys(t, g)
	if len(keys) != 3 {
		t.Fatalf("keys = %+v", keys)
	}
	want := []struct{ exp, label string }{{future.Format(time.RFC3339), "b1"}, {future.Format(time.RFC3339), "b2"}, {"", "b3"}}
	for i, w := range want {
		if keys[i].ExpiresAt != w.exp || keys[i].Label != w.label || keys[i].State != "ready" {
			t.Errorf("key %d = %+v, want %+v", i, keys[i], w)
		}
	}

	// 重复导入：默认计为 duplicate；--update 改写过期时间与标签
	later := future.Add(24 * time.Hour)
	renewed := write("renewed.jsonl",
		`{"api_key":"mk_SECRET_A1","expires_at":"`+later.Format(time.RFC3339)+`"}`,
		`{"api_key":"mk_SECRET_B2"}`,
		`{"api_key":"mk_SECRET_C3","expires_at":"`+past.Format(time.RFC3339)+`"}`,
	)
	if code, out, errOut := run("--file", renewed, "--expires-field", "expires_at"); code != 0 || !strings.Contains(out, "imported 0, duplicate 2, skipped 0 (filtered or invalid), failed 0, updated 0, expired 1") {
		t.Fatalf("re-import: %d %q %q", code, out, errOut)
	}
	if code, out, errOut := run("--file", renewed, "--expires-field", "expires_at", "--label", "renewed", "--update"); code != 0 || !strings.Contains(out, "imported 0, duplicate 0, skipped 0 (filtered or invalid), failed 0, updated 2, expired 1") {
		t.Fatalf("update: %d %q %q", code, out, errOut)
	}
	keys = listKeys(t, g)
	if keys[0].ExpiresAt != later.Format(time.RFC3339) || keys[0].Label != "renewed" || keys[1].ExpiresAt != "" || keys[1].Label != "renewed" || keys[2].Label != "b3" {
		t.Fatalf("after update = %+v", keys)
	}

	// --include-expired 导入已过期账号，管理面显示 expired
	if code, out, errOut := run("--file", renewed, "--expires-field", "expires_at", "--include-expired"); code != 0 || !strings.Contains(out, "imported 1, duplicate 2") {
		t.Fatalf("include-expired: %d %q %q", code, out, errOut)
	}
	keys = listKeys(t, g)
	if len(keys) != 4 || keys[3].State != "expired" || keys[3].ExpiresAt != past.Format(time.RFC3339) {
		t.Fatalf("expired key = %+v", keys)
	}

	for _, bad := range [][]string{
		{"--file", file, "--label", "x", "--label-field", "batch"},
		{"--file", file, "--format", "lines", "--expires-field", "expires_at"},
		{"--file", file, "--update"},
		{"--file", file, "--tz", "Mars/Olympus"},
		{"--file", file, "--label", strings.Repeat("x", 65)},
	} {
		if code, _, _ := run(bad...); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
	if strings.Contains(g.stderr.String(), "mk_SECRET") {
		t.Error("gateway log leaks imported keys")
	}
}

func TestParseExpiry(t *testing.T) {
	cst := time.FixedZone("UTC+08:00", 8*3600)
	want := time.Date(2026, 12, 1, 8, 30, 0, 0, time.UTC)
	for _, in := range []string{
		"2026-12-01T16:30:00+08:00", "2026-12-01T08:30:00Z", "2026-12-01T08:30:00.000Z",
		"1796113800", "1796113800000",
		"2026-12-01 16:30:00", "2026-12-01T16:30:00", "2026-12-01 16:30",
	} {
		got, err := parseExpiry(in, cst)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if got, err := parseExpiry("2026-12-01", cst); err != nil || !got.Equal(time.Date(2026, 11, 30, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("date only = %v, %v (want midnight in --tz)", got, err)
	}
	for _, in := range []string{"", "  ", "null"} {
		if got, err := parseExpiry(in, cst); err != nil || !got.IsZero() {
			t.Errorf("parseExpiry(%q) = %v, %v; want never", in, got, err)
		}
	}
	for _, in := range []string{"tomorrow", "0", "-5", "2026-13-01", "12/01/2026"} {
		if _, err := parseExpiry(in, cst); err == nil {
			t.Errorf("parseExpiry(%q) accepted", in)
		}
	}
}

func TestParseTZ(t *testing.T) {
	for in, off := range map[string]int{"UTC": 0, "+08:00": 8 * 3600, "-05:30": -(5*3600 + 30*60), "+00:00": 0} {
		loc, err := parseTZ(in)
		if err != nil {
			t.Fatalf("parseTZ(%q): %v", in, err)
		}
		if _, got := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Zone(); got != off {
			t.Errorf("parseTZ(%q) offset = %d, want %d", in, got, off)
		}
	}
	if loc, err := parseTZ(""); err != nil || loc != time.Local {
		t.Errorf("default tz = %v, %v", loc, err)
	}
	for _, in := range []string{"+25:00", "+08:60", "8", "Mars/Olympus"} {
		if _, err := parseTZ(in); err == nil {
			t.Errorf("parseTZ(%q) accepted", in)
		}
	}
}
