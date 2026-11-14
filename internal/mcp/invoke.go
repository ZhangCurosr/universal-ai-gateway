package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/files"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// listTools 实现 TL-01 ~ TL-04：返回客户端 p 可用的工具（按名称升序），转换为 MCP Tool 对象。
func (s *Server) listTools(p *auth.Principal) []map[string]any {
	var out []map[string]any
	for _, t := range s.d.Registry.Tools() {
		var desc string
		if t.Tool.Model != "" {
			// TL-03: Tool.Model 非空：只有该服务已配置（core.Route 存在）且客户端白名单允许时才列出
			if _, ok := s.d.Core.Route(t.Tool.Model); !ok {
				continue
			}
			if p != nil && !p.ModelAllowed(t.Tool.Model) {
				continue
			}
			desc = t.Tool.Description
		} else {
			// TL-04: Tool.Model 为空（服务由参数指定）：可用服务 = core.Models(principal) 中上游协议等于工具所属协议、
			// 或注册了「工具协议 → 上游协议」转换器的项。为空则不列出；否则在 description 末尾追加 "\n\nAvailable models: a, b, c"（按名称升序）
			avail := s.availableModels(t, p)
			if len(avail) == 0 {
				continue
			}
			desc = t.Tool.Description + "\n\nAvailable models: " + strings.Join(avail, ", ")
		}

		item := map[string]any{
			"name":        t.Tool.Name,
			"description": desc,
			"inputSchema": t.Tool.InputSchema,
			"annotations": map[string]any{
				"readOnlyHint":  t.Tool.ReadOnly,
				"openWorldHint": true,
			},
		}
		if t.Tool.Title != "" {
			item["title"] = t.Tool.Title
		}
		out = append(out, item)
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func (s *Server) availableModels(entry protocol.ToolEntry, p *auth.Principal) []string {
	if entry.Tool.Model != "" {
		if _, ok := s.d.Core.Route(entry.Tool.Model); !ok {
			return nil
		}
		if p != nil && !p.ModelAllowed(entry.Tool.Model) {
			return nil
		}
		return []string{entry.Tool.Model}
	}
	toolProto := entry.Ingress.Protocol()
	models := s.d.Core.Models(p)
	var avail []string
	for _, m := range models {
		if m.Protocol == toolProto {
			avail = append(avail, m.ID)
			continue
		}
		if _, ok := s.d.Registry.Translator(toolProto, m.Protocol); ok {
			avail = append(avail, m.ID)
		}
	}
	sort.Strings(avail)
	return avail
}

// callTool 实现 TL-05 ~ TL-10：把一次工具调用转换为内部请求，交给内核，再转换为工具结果。
// 返回的 *protocol.ToolResult 总是非 nil；只有工具不存在时返回 JSON-RPC 错误。
func (s *Server) callTool(r *http.Request, name string, args json.RawMessage) (*protocol.ToolResult, *Error) {
	// 1. entry, ok := Registry.Tool(name)；不存在 → &Error{CodeInvalidParams, "unknown tool"}（TL-05）。
	entry, ok := s.d.Registry.Tool(name)
	if !ok {
		return nil, &Error{Code: CodeInvalidParams, Message: "unknown tool"}
	}
	p, _ := auth.PrincipalFrom(r.Context())
	// 2. Tool.Model 非空且客户端不允许 → TextResult("model ... is not allowed", true)（TL-06）。
	if entry.Tool.Model != "" {
		if p != nil && !p.ModelAllowed(entry.Tool.Model) {
			return protocol.TextResult("model "+entry.Tool.Model+" is not allowed", true), nil
		}
		if _, ok := s.d.Core.Route(entry.Tool.Model); !ok {
			return protocol.TextResult("model "+entry.Tool.Model+" is not configured", true), nil
		}
	} else {
		if len(s.availableModels(entry, p)) == 0 {
			return nil, &Error{Code: CodeInvalidParams, Message: "unknown tool"}
		}
	}

	// 3. tr, err := entry.Source.ToolRequest(name, args)；err → TextResult(err.Error(), true)（TL-07）。
	tr, err := entry.Source.ToolRequest(name, args)
	if err != nil {
		return protocol.TextResult(err.Error(), true), nil
	}

	// 4. rt := 该入站适配器中 Op == tr.Op 的 RouteCall；找不到 → TextResult("internal error", true) 并记 Error 日志。
	var rt protocol.Route
	var found bool
	for _, route := range entry.Ingress.Routes() {
		if route.Op == tr.Op && route.Kind == protocol.RouteCall {
			rt = route
			found = true
			break
		}
	}
	if !found {
		s.d.Logger.Error("mcp: route not found for tool", "tool", name, "op", tr.Op)
		return protocol.TextResult("internal error", true), nil
	}

	// 5. req := newInternalRequest(r, rt, tr)；h := s.handlers[(协议, rt.Op)]；
	//    bw := newBufferedWriter(MaxResultBytes)；h.ServeHTTP(bw, req)（TL-08）。
	req, err := newInternalRequest(r, rt, tr)
	if err != nil {
		return protocol.TextResult(err.Error(), true), nil
	}
	// FS-20 / FS-22：工具调用不经过 HTTP 中间件，在这里把文件 ID 换成请求体
	var owner int64
	if p != nil {
		owner = p.ClientID
	}
	cleanup, err := files.Attach(s.d.Files, req, owner, rt.Body != protocol.BodyNone)
	if err != nil {
		return protocol.TextResult(apperr.Normalize(err).Message, true), nil
	}
	defer cleanup()

	k := handlerKey{proto: entry.Ingress.Protocol(), op: rt.Op}
	h := s.handlers[k]
	if h == nil {
		h = s.d.Core.Handler(entry.Ingress, rt)
	}

	bw := newBufferedWriter(s.d.MaxResultBytes)
	h.ServeHTTP(bw, req)

	// 6. bw.overflow → TextResult("result exceeds mcp.max_result_bytes", true)（TL-09）。
	if bw.overflow {
		return protocol.TextResult("result exceeds mcp.max_result_bytes", true), nil
	}

	// 7. res, err := entry.Source.ToolResult(name, bw.status, bw.header, bw.body.Bytes())；
	//    err → TextResult("could not parse the service response", true)（TL-10）。
	status := bw.status
	if status == 0 {
		status = http.StatusOK
	}
	res, err := entry.Source.ToolResult(name, status, bw.header, bw.body.Bytes())
	if err != nil {
		s.d.Logger.Warn("mcp: could not parse service response", "tool", name, "err", err)
		return protocol.TextResult("could not parse the service response", true), nil
	}
	if res == nil {
		res = protocol.TextResult("", false)
	}
	return res, nil
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
		if strings.Contains(rt.Path, "{"+k+"...}") {
			// TL-12：通配段的值是已转义的路径，PathValue 与 ServeMux 一致取解码后的值（fillPath 已校验可解码）
			if dec, err := url.PathUnescape(v); err == nil {
				v = dec
			}
		}
		req.SetPathValue(k, v)
	}
	return req, nil
}

// checkEscapedPath 校验 {name...} 的值是合法的已转义路径（TL-12）：可被 url.PathUnescape 解码，
// 且不含 ?、#、空白与控制字符。
func checkEscapedPath(v string) error {
	for _, c := range v {
		if c == '?' || c == '#' || c <= ' ' || c == 0x7f {
			return errInvalidEscapedPath
		}
	}
	if _, err := url.PathUnescape(v); err != nil {
		return errInvalidEscapedPath
	}
	return nil
}

var errInvalidEscapedPath = errors.New("invalid escaped path")

// fillPath 把 ServeMux 模式中的 {name} 与 {name...} 替换为 values 中的值（TL-12）。
// {name} 的值整体转义，不允许为空；{name...} 的值是已转义的路径，校验后原样写入，允许为空。
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
		raw := pattern[i+1 : i+j]
		name := strings.TrimSuffix(raw, "...")
		v, ok := values[name]
		b.WriteString(pattern[:i])
		if name != raw { // {name...}
			if err := checkEscapedPath(v); err != nil {
				return "", &Error{Code: CodeInvalidParams, Message: "invalid path parameter " + name}
			}
			b.WriteString(v)
		} else {
			if !ok || v == "" {
				return "", &Error{Code: CodeInvalidParams, Message: "missing path parameter " + name}
			}
			b.WriteString(url.PathEscape(v))
		}
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
