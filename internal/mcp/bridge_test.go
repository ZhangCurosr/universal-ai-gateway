package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 超过 MaxMessageBytes 的行被丢弃并回一条 id 为 null 的 -32600 错误，桥接继续处理后续消息；
// 最后一行没有换行符时也会被转发。
func TestBridgeOversizedLineContinues(t *testing.T) {
	var posts int
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":7,"result":{}}`) // 测试桩
	}))
	defer gw.Close()
	c := &Client{URL: gw.URL + "/mcp", Token: "uai_live_secret", HTTP: gw.Client()}

	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"x":"` + strings.Repeat("a", MaxMessageBytes) + `"}}`
	in := strings.NewReader(huge + "\n" + `{"jsonrpc":"2.0","id":7,"method":"ping"}`) // 最后一行无换行
	var out, log strings.Builder
	if err := Bridge(context.Background(), c, in, &out, &log); err != nil {
		t.Fatalf("Bridge() = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || posts != 1 {
		t.Fatalf("output = %q, posts = %d; want 2 lines and 1 forwarded message", out.String(), posts)
	}
	var first struct {
		ID    json.RawMessage `json:"id"`
		Error *Error          `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if string(first.ID) != "null" || first.Error == nil || first.Error.Code != CodeInvalidRequest {
		t.Errorf("oversized reply = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"id":7`) {
		t.Errorf("second reply = %s", lines[1])
	}
	if !strings.Contains(log.String(), "larger than") || strings.Contains(log.String(), "uai_live_secret") {
		t.Errorf("log = %q", log.String())
	}
}

func TestReadLine(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("ab\r\n"+strings.Repeat("x", 40)+"\nlast"), 16)
	if line, long, err := readLine(br, 10); string(line) != "ab" || long || err != nil {
		t.Fatalf("line 1 = %q, %v, %v", line, long, err)
	}
	if line, long, err := readLine(br, 10); line != nil || !long || err != nil {
		t.Fatalf("line 2 = %q, %v, %v", line, long, err)
	}
	if line, long, err := readLine(br, 10); string(line) != "last" || long || !errors.Is(err, io.EOF) {
		t.Fatalf("line 3 = %q, %v, %v", line, long, err)
	}
}
