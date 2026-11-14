package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// FS-41：gateway files upload / list / delete 与 gateway mcp --upload-dir 的参数检查。
func TestCmdFiles(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"file_abc","name":"a.txt","size":`+itoa(len(b))+`,"expires_at":"2026-11-15T08:00:00Z"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"files":[{"id":"file_abc","name":"a.txt","size":5,"expires_at":"2026-11-15T08:00:00Z"}],"used":{"files":1,"bytes":5}}`)
		case r.URL.Path == "/files/file_missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"file expired or not found"}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	env := func(k string) string {
		return map[string]string{envURL: srv.URL, envToken: "uai_live_secret"}[k]
	}
	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string, string) {
		var out, errOut strings.Builder
		code := cmdFiles(context.Background(), args, env, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run("upload", local, "--ttl", "2h"); code != 0 || !strings.HasPrefix(out, "file_abc  5 bytes") {
		t.Fatalf("upload: %d %q", code, out)
	}
	if code, out, _ := run("upload", "--json", local); code != 0 || !strings.Contains(out, `"id":"file_abc"`) {
		t.Fatalf("upload --json: %d %q", code, out)
	}
	if code, out, _ := run("list"); code != 0 || !strings.Contains(out, "a.txt") || !strings.Contains(out, "1 files, 5 bytes") {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, out, _ := run("delete", "file_abc"); code != 0 || out != "deleted file_abc\n" {
		t.Fatalf("delete: %d %q", code, out)
	}
	if code, _, errOut := run("delete", "file_missing"); code != 1 || !strings.Contains(errOut, "HTTP 404: file expired or not found") || strings.Contains(errOut, "uai_live_secret") {
		t.Fatalf("delete missing: %d %q", code, errOut)
	}
	if !strings.Contains(seen[0], "ttl=2h0m0s") || !strings.Contains(seen[0], "name=a.txt") {
		t.Fatalf("upload query = %q", seen[0])
	}
	for _, args := range [][]string{{}, {"upload"}, {"delete"}, {"bogus"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	var errOut strings.Builder
	if code := cmdMCP(context.Background(), []string{"--upload-dir", filepath.Join(t.TempDir(), "missing")}, env, strings.NewReader(""), io.Discard, &errOut); code != 2 || !strings.Contains(errOut.String(), "--upload-dir") {
		t.Fatalf("mcp --upload-dir missing: %d %q", code, errOut.String())
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
