package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeFilesGateway struct {
	mu       sync.Mutex
	mcpPosts int
	uploads  []string // name 查询参数
	bodies   []string
	fail     bool
}

func (f *fakeFilesGateway) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mcp":
			f.mcpPosts++
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &msg) // 测试桩
			if msg.Method == "tools/list" {
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":{"tools":[{"name":"remote_tool"}]}}`)
				return
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":{"content":[{"type":"text","text":"remote"}],"isError":false}}`)
		case "/files":
			if r.Header.Get("Authorization") != "Bearer uai_live_secret" || r.ContentLength <= 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if f.fail {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"message":"this client already keeps 100 files"}}`)
				return
			}
			b, _ := io.ReadAll(r.Body)
			f.uploads = append(f.uploads, r.URL.Query().Get("name")+"|"+r.URL.Query().Get("ttl")+"|"+r.Header.Get("Content-Type"))
			f.bodies = append(f.bodies, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"file_x","name":"a.pdf","size":`+jsonInt(len(b))+`,"sha256":"00","content_type":"application/pdf"}`)
		}
	})
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func runBridge(t *testing.T, c *Client, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := Bridge(context.Background(), c, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad line %q", l)
		}
		res = append(res, m)
	}
	return res
}

func resultText(m map[string]any) (string, bool) {
	r := m["result"].(map[string]any)
	return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
}

func toolNames(m map[string]any) []string {
	var names []string
	for _, t := range m["result"].(map[string]any)["tools"].([]any) {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	return names
}

const (
	bridgeListMsg = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
)

func uploadMsg(path, ttl string) string {
	args, _ := json.Marshal(map[string]string{"path": path, "ttl": ttl})
	return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"gateway_upload_file","arguments":` + string(args) + `}}`
}

// FS-40：没有 --upload-dir 时不提供上传工具，消息照常转发。
func TestBridgeWithoutUploadDirs(t *testing.T) {
	fg := &fakeFilesGateway{}
	gw := httptest.NewServer(fg.handler())
	defer gw.Close()
	c := &Client{URL: gw.URL + "/mcp", Token: "uai_live_secret", HTTP: gw.Client()}
	res := runBridge(t, c, bridgeListMsg, uploadMsg("x", ""))
	if names := toolNames(res[0]); strings.Join(names, ",") != "remote_tool" {
		t.Fatalf("tools = %v", names)
	}
	if text, _ := resultText(res[1]); text != "remote" || fg.mcpPosts != 2 || len(fg.uploads) != 0 {
		t.Fatalf("upload call was not forwarded: %q posts=%d", text, fg.mcpPosts)
	}
}

func TestBridgeUploadTool(t *testing.T) {
	fg := &fakeFilesGateway{}
	gw := httptest.NewServer(fg.handler())
	defer gw.Close()
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	if err := os.MkdirAll(filepath.Join(allowed, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{filepath.Join(allowed, "sub", "a.pdf"): "%PDF", filepath.Join(root, "secret.jsonl"): "keys", filepath.Join(allowed, "empty.txt"): ""} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dirs, err := NewUploadDirs([]string{allowed})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{URL: gw.URL + "/mcp", Token: "uai_live_secret", HTTP: gw.Client(), UploadDirs: dirs}
	res := runBridge(t, c,
		bridgeListMsg,
		uploadMsg(filepath.Join(allowed, "sub", "a.pdf"), "2h"),
		uploadMsg(filepath.Join(allowed, "sub", "..", "..", "secret.jsonl"), ""),
		uploadMsg(filepath.Join(allowed, "missing.pdf"), ""),
		uploadMsg(filepath.Join(allowed, "empty.txt"), ""),
		uploadMsg(filepath.Join(allowed, "sub", "a.pdf"), "soon"),
	)
	if names := toolNames(res[0]); strings.Join(names, ",") != "remote_tool,gateway_upload_file" {
		t.Fatalf("tools = %v", names)
	}
	if text, isErr := resultText(res[1]); isErr || !strings.Contains(text, `"id":"file_x"`) {
		t.Fatalf("upload: %q", text)
	}
	if len(fg.uploads) != 1 || fg.uploads[0] != "a.pdf|2h0m0s|application/pdf" || fg.bodies[0] != "%PDF" {
		t.Fatalf("gateway saw %v %v", fg.uploads, fg.bodies)
	}
	for i, want := range map[int]string{2: "outside the directories", 3: "cannot open", 4: "is empty", 5: "ttl must be"} {
		if text, isErr := resultText(res[i]); !isErr || !strings.Contains(text, want) {
			t.Errorf("message %d: %q (want %q)", i, text, want)
		}
	}
	if fg.mcpPosts != 1 {
		t.Fatalf("local tool calls were forwarded: %d", fg.mcpPosts)
	}

	// 网关拒绝时把消息交给 AI
	fg.fail = true
	res = runBridge(t, c, uploadMsg(filepath.Join(allowed, "sub", "a.pdf"), ""))
	if text, isErr := resultText(res[0]); !isErr || !strings.Contains(text, "HTTP 409: this client already keeps 100 files") {
		t.Fatalf("gateway error: %q", text)
	}
}

// 符号链接指向允许目录之外时拒绝（创建符号链接需要权限时跳过）。
func TestUploadDirsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowed, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	dirs, err := NewUploadDirs([]string{allowed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dirs.Resolve(link); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("symlink escape: %v", err)
	}
	if _, err := NewUploadDirs([]string{secret}); err == nil {
		t.Fatal("a file accepted as --upload-dir")
	}
	if _, err := dirs.Resolve(""); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestAppendUploadToolKeepsUnexpectedShapes(t *testing.T) {
	for _, body := range []string{`not json`, `{"jsonrpc":"2.0","id":1,"error":{"code":-1}}`, `{"result":{"tools":"x"}}`} {
		if got := string(appendUploadTool([]byte(body))); got != body {
			t.Errorf("%s → %s", body, got)
		}
	}
}
