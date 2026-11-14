package rest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// ToolDef 是一个接口声明在适配器侧的表示（RS-40，配置见 spec/03 CFG-17）。
// 装配代码负责把配置转换为 ToolDef；配置层已校验名称、方法、路径与 Schema 的形状。
type ToolDef struct {
	Service     string          // 所属服务名（config models[].name），即工具的 Model
	Name        string          // 工具名
	Title       string          // 可选
	Description string          // 给 AI 看的说明
	Method      string          // GET | DELETE | POST | PUT | PATCH（大写）
	Path        string          // 以 / 开头，相对于池的 base_url，可含 {name} 占位符
	InputSchema json.RawMessage // type 为 object 的 JSON Schema
	Idempotent  bool            // 只对 POST / PATCH 有意义：选用可重试的 /rest-idem/ 路由
	ReadOnly    bool            // MCP readOnlyHint
	// FileArg 非空时（T69，FS-30）：该参数是文件 ID，网关用文件内容作为请求体；其余参数进查询串。
	FileArg string
	// FileNameQuery 非空时把文件名填进这个查询参数（FS-21）。
	FileNameQuery string
}

// 文件中转的内部请求头（spec/08 FS-20、FS-21）；由网关在交给适配器之前处理并删除。
const (
	headerFile          = "X-UAIG-File"
	headerFileNameQuery = "X-UAIG-File-Name-Query"
)

// toolTable 是构造后只读的工具表（RS-40）：Ingress 持有指向它的指针，因此仍然无状态、并发安全。
type toolTable struct {
	list   []protocol.Tool
	byName map[string]toolEntry
}

type toolEntry struct {
	def          ToolDef
	placeholders map[string]bool
}

// maxToolErrorBytes 是非 2xx 时放进工具结果的响应体字节数上限（RS-43）。
const maxToolErrorBytes = 2000

func newToolTable(defs []ToolDef) (*toolTable, error) {
	t := &toolTable{byName: make(map[string]toolEntry, len(defs))}
	for _, d := range defs {
		if d.Service == "" || d.Name == "" {
			return nil, fmt.Errorf("rest: tool %q: service and name are required", d.Name)
		}
		if _, dup := t.byName[d.Name]; dup {
			return nil, fmt.Errorf("rest: duplicate tool %q", d.Name)
		}
		switch d.Method {
		case http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodPost, http.MethodPatch:
		default:
			return nil, fmt.Errorf("rest: tool %q: unsupported method %q", d.Name, d.Method)
		}
		if !strings.HasPrefix(d.Path, "/") {
			return nil, fmt.Errorf("rest: tool %q: path must start with /", d.Name)
		}
		names, err := placeholderNames(d.Path)
		if err != nil {
			return nil, fmt.Errorf("rest: tool %q: %w", d.Name, err)
		}
		ph := make(map[string]bool, len(names))
		for _, n := range names {
			ph[n] = true
		}
		t.byName[d.Name] = toolEntry{def: d, placeholders: ph}
		t.list = append(t.list, protocol.Tool{
			Name:        d.Name,
			Title:       d.Title,
			Description: d.Description,
			InputSchema: d.InputSchema,
			Model:       d.Service,
			ReadOnly:    d.ReadOnly,
		})
	}
	sort.Slice(t.list, func(i, j int) bool { return t.list[i].Name < t.list[j].Name })
	return t, nil
}

// placeholderNames 返回 path 中 {name} 占位符的名字；花括号不成对时返回错误。
func placeholderNames(path string) ([]string, error) {
	var names []string
	for rest := path; ; {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			if strings.IndexByte(rest, '}') >= 0 {
				return nil, fmt.Errorf("unbalanced braces in path")
			}
			return names, nil
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 || strings.IndexByte(rest[:i], '}') >= 0 {
			return nil, fmt.Errorf("unbalanced braces in path")
		}
		name := rest[i+1 : i+j]
		if name == "" || strings.ContainsAny(name, "{./") {
			return nil, fmt.Errorf("invalid placeholder {%s}", name)
		}
		names = append(names, name)
		rest = rest[i+j+1:]
	}
}

// Tools 返回配置声明的工具，按名称升序（RS-41）。零值 Ingress 没有工具。
func (in Ingress) Tools() []protocol.Tool {
	if in.tools == nil {
		return nil
	}
	return append([]protocol.Tool(nil), in.tools.list...)
}

// ToolRequest 把工具参数映射为对 rest 路由的请求（RS-42，spec/08 §8.3.5）。
func (in Ingress) ToolRequest(name string, args json.RawMessage) (*protocol.ToolRequest, error) {
	if in.tools == nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool "+name)
	}
	e, ok := in.tools.byName[name]
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool "+name)
	}
	m, err := decodeArgs(args)
	if err != nil {
		return nil, err
	}

	path, err := fillToolPath(e.def.Path, m)
	if err != nil {
		return nil, err
	}

	tr := &protocol.ToolRequest{PathValues: map[string]string{"service": e.def.Service, "path": strings.TrimPrefix(path, "/")}}
	if e.def.FileArg != "" {
		return fileToolRequest(tr, e.def, m)
	}
	switch e.def.Method {
	case http.MethodGet, http.MethodDelete:
		q, err := argsToQuery(m)
		if err != nil {
			return nil, err
		}
		tr.Query = q
		tr.Op = OpGet
		if e.def.Method == http.MethodDelete {
			tr.Op = OpDelete
		}
	default:
		body, err := json.Marshal(m) // map 的键按字典序输出；RawMessage 保留数字原文
		if err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "arguments cannot be encoded as JSON")
		}
		tr.Body = body
		switch {
		case e.def.Method == http.MethodPut:
			tr.Op = OpPut
		case e.def.Method == http.MethodPost && e.def.Idempotent:
			tr.Op = OpPostIdem
		case e.def.Method == http.MethodPost:
			tr.Op = OpPost
		case e.def.Idempotent:
			tr.Op = OpPatchIdem
		default:
			tr.Op = OpPatch
		}
	}
	return tr, nil
}

// fileToolRequest 实现 FS-30：文件 ID 放进 X-UAIG-File，其余参数按 GET 的规则进查询串，请求体为空。
func fileToolRequest(tr *protocol.ToolRequest, def ToolDef, m map[string]json.RawMessage) (*protocol.ToolRequest, error) {
	var id string
	if raw, ok := m[def.FileArg]; !ok || json.Unmarshal(raw, &id) != nil || strings.TrimSpace(id) == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("argument %q must be a file_id string (upload the file first)", def.FileArg))
	}
	delete(m, def.FileArg)
	q, err := argsToQuery(m)
	if err != nil {
		return nil, err
	}
	tr.Query = q
	tr.Header = http.Header{}
	tr.Header.Set(headerFile, strings.TrimSpace(id))
	if def.FileNameQuery != "" {
		tr.Header.Set(headerFileNameQuery, def.FileNameQuery)
	}
	switch {
	case def.Method == http.MethodPut:
		tr.Op = OpPut
	case def.Idempotent:
		tr.Op = OpPostIdem
	default:
		tr.Op = OpPost
	}
	return tr, nil
}

// decodeArgs 把参数解析为 名称 → 原始 JSON 值；缺省或 null 视为空对象。
func decodeArgs(args json.RawMessage) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(args)
	m := map[string]json.RawMessage{}
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return m, nil
	}
	if trimmed[0] != '{' {
		return nil, apperr.New(apperr.CodeInvalidRequest, "arguments must be a JSON object")
	}
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "arguments must be a JSON object")
	}
	return m, nil
}

// fillToolPath 用参数填写 path 中的占位符（按单个路径段转义），并把用到的参数移出 m。
func fillToolPath(path string, m map[string]json.RawMessage) (string, error) {
	var b strings.Builder
	for rest := path; ; {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		j := strings.IndexByte(rest[i:], '}')
		name := rest[i+1 : i+j]
		raw, ok := m[name]
		if !ok {
			return "", apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("argument %q is required", name))
		}
		v, kind := scalarText(raw)
		if kind != kindString && kind != kindNumber {
			return "", apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("argument %q must be a string or number", name))
		}
		if v == "" || v == "." || v == ".." {
			return "", apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("argument %q must not be empty, \".\" or \"..\"", name))
		}
		delete(m, name)
		b.WriteString(rest[:i])
		b.WriteString(url.PathEscape(v))
		rest = rest[i+j+1:]
	}
}

// argsToQuery 把剩余参数放进查询串（RS-42）：标量各一个值，标量数组重复同名参数，null 省略。
func argsToQuery(m map[string]json.RawMessage) (url.Values, error) {
	q := url.Values{}
	for k, raw := range m {
		v, kind := scalarText(raw)
		switch kind {
		case kindNull:
		case kindString, kindNumber, kindBool:
			q.Add(k, v)
		case kindArray:
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("argument %q is not valid JSON", k))
			}
			for _, it := range items {
				iv, ik := scalarText(it)
				switch ik {
				case kindNull:
				case kindString, kindNumber, kindBool:
					q.Add(k, iv)
				default:
					return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("argument %q must be a scalar or an array of scalars", k))
				}
			}
		default:
			return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("argument %q must be a scalar or an array of scalars", k))
		}
	}
	if len(q) == 0 {
		return nil, nil
	}
	return q, nil
}

type jsonKind int

const (
	kindInvalid jsonKind = iota
	kindNull
	kindString
	kindNumber
	kindBool
	kindArray
	kindObject
)

// scalarText 判断原始 JSON 值的类型；字符串返回解码后的值，数字与布尔返回原文。
func scalarText(raw json.RawMessage) (string, jsonKind) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "", kindInvalid
	}
	switch c := t[0]; {
	case c == '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return "", kindInvalid
		}
		return s, kindString
	case c == '-' || (c >= '0' && c <= '9'):
		return string(t), kindNumber
	case c == 't' || c == 'f':
		return string(t), kindBool
	case c == 'n':
		return "", kindNull
	case c == '[':
		return "", kindArray
	case c == '{':
		return "", kindObject
	}
	return "", kindInvalid
}

// ToolResult 把 rest 路由的完整响应转换为工具结果（RS-43）。不返回 error。
func (in Ingress) ToolResult(_ string, status int, header http.Header, body []byte) (*protocol.ToolResult, error) {
	if status < 200 || status > 299 {
		return protocol.TextResult(fmt.Sprintf("HTTP %d: %s", status, truncateUTF8(body, maxToolErrorBytes)), true), nil
	}
	if len(body) == 0 {
		return protocol.TextResult(fmt.Sprintf("empty response (HTTP %d)", status), false), nil
	}
	ct := header.Get("Content-Type")
	mt := ""
	if ct != "" {
		if parsed, _, err := mime.ParseMediaType(ct); err == nil {
			mt = strings.ToLower(parsed)
		} else {
			mt = "invalid"
		}
	}
	isJSON := mt == "application/json" || strings.HasSuffix(mt, "+json")
	// 没有 Content-Type 时按文本处理（不少 API 不设置它）；是合法 JSON 时同样提供结构化结果
	textual := isJSON || strings.HasPrefix(mt, "text/") || mt == ""
	if !textual || !utf8.Valid(body) {
		if ct == "" {
			ct = "unknown type"
		}
		return protocol.TextResult(fmt.Sprintf("binary response (%s, %d bytes) is not supported by tools", ct, len(body)), true), nil
	}
	res := protocol.TextResult(string(body), false)
	if (isJSON || mt == "") && json.Valid(body) {
		if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '{' {
			res.Structured = json.RawMessage(t)
		}
	}
	return res, nil
}

// truncateUTF8 取 b 的前 n 字节（在 UTF-8 字符边界截断），非法字节替换为 U+FFFD。
func truncateUTF8(b []byte, n int) string {
	if len(b) > n {
		for n > 0 && !utf8.RuneStart(b[n]) {
			n--
		}
		b = b[:n]
	}
	return strings.ToValidUTF8(string(b), "�")
}
