package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// UploadToolName 是桥接提供的本地上传工具（FS-40）。
const UploadToolName = "gateway_upload_file"

// uploadToolJSON 是 tools/list 中追加的工具对象（FS-40）。
const uploadToolJSON = `{"name":"` + UploadToolName + `","title":"Upload a local file",` +
	`"description":"Upload a local file to the gateway and get a file_id that other tools accept in place of file content. ` +
	`Only files inside the directories the user allowed with --upload-dir can be uploaded. Call this tool directly (not through gateway_call_tool).",` +
	`"inputSchema":{"type":"object","properties":{"path":{"type":"string","description":"Path of the local file"},` +
	`"ttl":{"type":"string","description":"How long the gateway keeps the file, such as 30m or 24h (default: the gateway's files.default_ttl)"}},` +
	`"required":["path"],"additionalProperties":false},` +
	`"annotations":{"readOnlyHint":false,"openWorldHint":false}}`

// uploadReadPerMiB 与网关侧相同：上传超时 = 60 秒 + 每 MiB 10 秒。
const uploadPerMiB = 10 * time.Second

// FileInfo 是 POST /files 与 GET /files/{id} 的响应（FS-10）。
type FileInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// filesURL 由 MCP 端点地址得到数据面 /files 地址。
func (c *Client) filesURL() string {
	return strings.TrimSuffix(strings.TrimRight(c.URL, "/"), "/mcp") + "/files"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// do 发送一个 /files 请求，返回 2xx 响应体（最多 MaxMessageBytes）；非 2xx 返回 *apperr.Error（取网关错误的 message）。
func (c *Client) do(req *http.Request) ([]byte, error) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, apperr.New(apperr.CodeUpstreamUnavailable, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxMessageBytes))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstreamError, "read response body", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	msg := http.StatusText(resp.StatusCode)
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	return nil, apperr.New(codeFromStatus(resp.StatusCode), fmt.Sprintf("HTTP %d: %s", resp.StatusCode, c.redact(msg)))
}

// UploadFile 把本地文件 path 上传到网关（FS-10）。ttl 为 0 时用网关的缺省值。
func (c *Client) UploadFile(ctx context.Context, path string, ttl time.Duration) (*FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("%s is empty", filepath.Base(path))
	}
	q := url.Values{"name": {filepath.Base(path)}}
	if ttl > 0 {
		q.Set("ttl", ttl.String())
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute+time.Duration(st.Size()>>20+1)*uploadPerMiB)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.filesURL()+"?"+q.Encode(), f)
	if err != nil {
		return nil, err
	}
	req.ContentLength = st.Size()
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	req.Header.Set("Content-Type", ct)
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var info FileInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstreamError, "could not parse the gateway response", err)
	}
	return &info, nil
}

// ListFiles 返回 GET /files 的原始 JSON（FS-11）。
func (c *Client) ListFiles(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.filesURL(), nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

// DeleteFile 删除一个文件（FS-11）。
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.filesURL()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	_, err = c.do(req)
	return err
}

// UploadDirs 是 FS-40 允许上传的目录（已解析为绝对路径并解开符号链接）。
type UploadDirs []string

// NewUploadDirs 解析 --upload-dir 的值：每个目录必须存在。
func NewUploadDirs(dirs []string) (UploadDirs, error) {
	var out UploadDirs
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err == nil {
			abs, err = filepath.EvalSymlinks(abs)
		}
		if err != nil {
			return nil, fmt.Errorf("--upload-dir %s: %w", d, err)
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("--upload-dir %s is not a directory", d)
		}
		out = append(out, abs)
	}
	return out, nil
}

// Resolve 把 path 解析为绝对路径并解开符号链接，必须位于某个允许的目录之内（FS-40）。
func (u UploadDirs) Resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return "", fmt.Errorf("cannot open %s", filepath.Base(path))
	}
	for _, d := range u {
		rel, err := filepath.Rel(d, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return abs, nil
		}
	}
	return "", errors.New("path is outside the directories allowed with --upload-dir")
}

// bridgeLocal 处理桥接自己执行的消息（FS-40）。handled 为 false 时照常转发；
// listTools 为 true 时表示这是 tools/list，转发后需要追加上传工具。
func (c *Client) bridgeLocal(ctx context.Context, line []byte) (resp []byte, handled, listTools bool) {
	if len(c.UploadDirs) == 0 {
		return nil, false, false
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return nil, false, false
	}
	switch {
	case msg.Method == "tools/list":
		return nil, false, true
	case msg.Method != "tools/call" || msg.Params.Name != UploadToolName || len(msg.ID) == 0:
		return nil, false, false
	}
	result := c.uploadTool(ctx, msg.Params.Arguments)
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	if err != nil {
		return nil, false, false
	}
	return b, true, false
}

func (c *Client) uploadTool(ctx context.Context, args json.RawMessage) map[string]any {
	fail := func(msg string) map[string]any {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": c.redact(msg)}}, "isError": true}
	}
	var a struct {
		Path string `json:"path"`
		TTL  string `json:"ttl"`
	}
	if len(args) > 0 && json.Unmarshal(args, &a) != nil {
		return fail("arguments must be an object {path, ttl?}")
	}
	var ttl time.Duration
	if a.TTL != "" {
		d, err := time.ParseDuration(a.TTL)
		if err != nil || d <= 0 {
			return fail("ttl must be a duration such as 30m or 24h")
		}
		ttl = d
	}
	path, err := c.UploadDirs.Resolve(a.Path)
	if err != nil {
		return fail(err.Error())
	}
	info, err := c.UploadFile(ctx, path, ttl)
	if err != nil {
		msg := err.Error()
		if ap, ok := apperr.As(err); ok {
			msg = ap.Message
		} else if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			msg = "cannot open " + filepath.Base(path)
		}
		return fail(msg)
	}
	text, err := json.Marshal(info)
	if err != nil {
		return fail("internal error")
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": info, "isError": false}
}

// appendUploadTool 在 tools/list 的结果末尾追加上传工具；响应不是预期的形状时原样返回。
func appendUploadTool(body []byte) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(body, &msg) != nil || msg["result"] == nil {
		return body
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(msg["result"], &result) != nil {
		return body
	}
	var tools []json.RawMessage
	if json.Unmarshal(result["tools"], &tools) != nil {
		return body
	}
	tools = append(tools, json.RawMessage(uploadToolJSON))
	var err error
	if result["tools"], err = json.Marshal(tools); err != nil {
		return body
	}
	if msg["result"], err = json.Marshal(result); err != nil {
		return body
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	return bytes.TrimSpace(out)
}
