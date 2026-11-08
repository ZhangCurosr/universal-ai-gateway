package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// listTools 实现 TL-01 ~ TL-04：返回客户端 p 可用的工具（按名称升序），转换为 MCP Tool 对象。
func (s *Server) listTools(p *auth.Principal) []map[string]any {
	// TODO(T32): 遍历 Registry.Tools()；Tool.Model 非空时要求 core.Route(Model) 存在且 p.ModelAllowed(Model)（TL-03）；
	// Tool.Model 为空时，可用服务 = core.Models(p) 中 上游协议 == 工具所属协议 或 注册了 (工具协议 → 上游协议) 转换器 的项，
	// 为空则不列出该工具，否则在 description 末尾追加 "\n\nAvailable models: a, b, c"（TL-04）。
	return nil
}

// callTool 实现 TL-05 ~ TL-10：把一次工具调用转换为内部请求，交给内核，再转换为工具结果。
// 返回的 *protocol.ToolResult 总是非 nil；只有工具不存在时返回 JSON-RPC 错误。
func (s *Server) callTool(r *http.Request, name string, args json.RawMessage) (*protocol.ToolResult, *Error) {
	// TODO(T32):
	//   1. entry, ok := Registry.Tool(name)；不存在 → &Error{CodeInvalidParams, "unknown tool"}（TL-05）。
	//   2. Tool.Model 非空且客户端不允许 → TextResult("model ... is not allowed", true)（TL-06）。
	//   3. tr, err := entry.Source.ToolRequest(name, args)；err → TextResult(err.Error(), true)（TL-07）。
	//   4. rt := 该入站适配器中 Op == tr.Op 的 RouteCall；找不到 → TextResult("internal error", true) 并记 Error 日志。
	//   5. req := newInternalRequest(r, rt, tr)；h := s.handlers[(协议, rt.Op)]；
	//      bw := newBufferedWriter(MaxResultBytes)；h.ServeHTTP(bw, req)（TL-08）。
	//   6. bw.overflow → TextResult("result exceeds mcp.max_result_bytes", true)（TL-09）。
	//   7. res, err := entry.Source.ToolResult(name, bw.status, bw.header, bw.body.Bytes())；
	//      err → TextResult("could not parse the service response", true)（TL-10）。
	return protocol.TextResult("not implemented (T32)", true), nil
}

// newInternalRequest 构造交给内核的内部请求（TL-08，已实现）：
// context 沿用外层 MCP 请求（Principal、请求 ID、客户端断开都随之传递）；
// 路径由 rt.Path 填入 tr.PathValues 得到，并通过 SetPathValue 设置，使适配器的 r.PathValue 可用；
// 只使用 tr.Header，不复制外层请求的任何头（外层的 Authorization 是网关 Token，不应进入适配器）。
func newInternalRequest(outer *http.Request, rt protocol.Route, tr *protocol.ToolRequest) (*http.Request, error) {
	path, err := fillPath(rt.Path, tr.PathValues)
	if err != nil {
		return nil, err
	}
	if len(tr.Query) > 0 {
		path += "?" + tr.Query.Encode()
	}
	req, err := http.NewRequestWithContext(outer.Context(), rt.Method, path, bytes.NewReader(tr.Body))
	if err != nil {
		return nil, err
	}
	req.RemoteAddr = outer.RemoteAddr
	for k, vs := range tr.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if len(tr.Body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range tr.PathValues {
		req.SetPathValue(k, v)
	}
	return req, nil
}

// fillPath 把 ServeMux 模式中的 {name} 与 {name...} 替换为 values 中的值（已转义）。缺少值时返回错误。
func fillPath(pattern string, values map[string]string) (string, error) {
	var b strings.Builder
	for {
		i := strings.IndexByte(pattern, '{')
		if i < 0 {
			b.WriteString(pattern)
			return b.String(), nil
		}
		j := strings.IndexByte(pattern[i:], '}')
		if j < 0 {
			return "", &Error{Code: CodeInternalError, Message: "malformed route pattern"}
		}
		name := strings.TrimSuffix(pattern[i+1:i+j], "...")
		v, ok := values[name]
		if !ok || v == "" {
			return "", &Error{Code: CodeInvalidParams, Message: "missing path parameter " + name}
		}
		b.WriteString(pattern[:i])
		b.WriteString(url.PathEscape(v))
		pattern = pattern[i+j+1:]
	}
}

// bufferedWriter 是有上限的 http.ResponseWriter（已实现）：收集内核的完整响应供 ToolResult 解析。
// 超过上限后丢弃后续数据并置 overflow；Flush 为空操作（实现 http.Flusher，使事件流也能写入）。
type bufferedWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int64
	overflow bool
}

func newBufferedWriter(limit int64) *bufferedWriter {
	return &bufferedWriter{header: http.Header{}, limit: limit}
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if room := b.limit - int64(b.body.Len()); int64(len(p)) > room {
		b.overflow = true
		if room > 0 {
			b.body.Write(p[:room])
		}
		return len(p), nil // 假装写成功：让内核正常收尾（结算），由 overflow 决定结果
	}
	return b.body.Write(p)
}

func (b *bufferedWriter) Flush() {}
