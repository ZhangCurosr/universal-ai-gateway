package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func testIngress(t *testing.T, defs ...ToolDef) Ingress {
	t.Helper()
	tt, err := newToolTable(defs)
	if err != nil {
		t.Fatalf("newToolTable: %v", err)
	}
	return Ingress{tools: tt}
}

var schema = json.RawMessage(`{"type":"object"}`)

func TestZeroIngressHasNoTools(t *testing.T) {
	if got := (Ingress{}).Tools(); len(got) != 0 {
		t.Fatalf("Tools() = %v", got)
	}
	if _, err := (Ingress{}).ToolRequest("x", nil); err == nil {
		t.Fatal("ToolRequest on zero Ingress succeeded")
	}
}

func TestToolsListing(t *testing.T) {
	in := testIngress(t,
		ToolDef{Service: "svc", Name: "b_tool", Description: "b", Method: "POST", Path: "/b", InputSchema: schema},
		ToolDef{Service: "svc", Name: "a_tool", Title: "A", Description: "a", Method: "GET", Path: "/a", InputSchema: schema, ReadOnly: true},
	)
	tools := in.Tools()
	if len(tools) != 2 || tools[0].Name != "a_tool" || tools[1].Name != "b_tool" {
		t.Fatalf("Tools() = %+v", tools)
	}
	if tools[0].Model != "svc" || !tools[0].ReadOnly || tools[0].Title != "A" || tools[1].ReadOnly {
		t.Errorf("tool fields = %+v", tools)
	}
	tools[0].Name = "mutated"
	if in.Tools()[0].Name != "a_tool" {
		t.Error("Tools() exposes the internal slice")
	}
}

func TestNewToolTableRejects(t *testing.T) {
	ok := ToolDef{Service: "s", Name: "t", Method: "GET", Path: "/x"}
	cases := []ToolDef{
		{Name: "t", Method: "GET", Path: "/x"},
		{Service: "s", Name: "t", Method: "TRACE", Path: "/x"},
		{Service: "s", Name: "t", Method: "GET", Path: "x"},
		{Service: "s", Name: "t", Method: "GET", Path: "/{a"},
		{Service: "s", Name: "t", Method: "GET", Path: "/a}"},
		{Service: "s", Name: "t", Method: "GET", Path: "/{a...}"},
	}
	for _, d := range cases {
		if _, err := newToolTable([]ToolDef{d}); err == nil {
			t.Errorf("newToolTable(%+v) accepted", d)
		}
	}
	if _, err := newToolTable([]ToolDef{ok, ok}); err == nil {
		t.Error("duplicate tool accepted")
	}
}

func TestToolRequestGET(t *testing.T) {
	in := testIngress(t, ToolDef{Service: "gh", Name: "issue", Method: "GET", Path: "/repos/{owner}/{repo}/issues/{number}"})
	tr, err := in.ToolRequest("issue", json.RawMessage(`{"owner":"a/b","repo":"r x","number":42,"state":"open","labels":["x","y",null],"page":2,"draft":false,"skip":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Op != OpGet || tr.PathValues["service"] != "gh" {
		t.Errorf("op/service = %q %q", tr.Op, tr.PathValues["service"])
	}
	if got := tr.PathValues["path"]; got != "repos/a%2Fb/r%20x/issues/42" {
		t.Errorf("path = %q", got)
	}
	want := url.Values{"state": {"open"}, "labels": {"x", "y"}, "page": {"2"}, "draft": {"false"}}
	if tr.Query.Encode() != want.Encode() {
		t.Errorf("query = %q, want %q", tr.Query.Encode(), want.Encode())
	}
	if tr.Body != nil {
		t.Errorf("GET body = %s", tr.Body)
	}
}

func TestToolRequestDELETEAndRoot(t *testing.T) {
	in := testIngress(t,
		ToolDef{Service: "s", Name: "del", Method: "DELETE", Path: "/items/{id}"},
		ToolDef{Service: "s", Name: "root", Method: "GET", Path: "/"},
	)
	tr, err := in.ToolRequest("del", json.RawMessage(`{"id":"7"}`))
	if err != nil || tr.Op != OpDelete || tr.PathValues["path"] != "items/7" || tr.Query != nil {
		t.Fatalf("del = %+v, %v", tr, err)
	}
	tr, err = in.ToolRequest("root", nil)
	if err != nil || tr.PathValues["path"] != "" {
		t.Fatalf("root = %+v, %v", tr, err)
	}
}

func TestToolRequestBodyMethods(t *testing.T) {
	in := testIngress(t,
		ToolDef{Service: "s", Name: "post", Method: "POST", Path: "/q/{id}"},
		ToolDef{Service: "s", Name: "post_idem", Method: "POST", Path: "/q", Idempotent: true},
		ToolDef{Service: "s", Name: "patch", Method: "PATCH", Path: "/q"},
		ToolDef{Service: "s", Name: "patch_idem", Method: "PATCH", Path: "/q", Idempotent: true},
		ToolDef{Service: "s", Name: "put", Method: "PUT", Path: "/q"},
	)
	tr, err := in.ToolRequest("post", json.RawMessage(`{"id":"x","n":12345678901234567890,"z":{"k":[1,2]},"a":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Op != OpPost || tr.PathValues["path"] != "q/x" {
		t.Errorf("op/path = %q %q", tr.Op, tr.PathValues["path"])
	}
	if got := string(tr.Body); got != `{"a":"s","n":12345678901234567890,"z":{"k":[1,2]}}` {
		t.Errorf("body = %s", got)
	}
	ops := map[string]string{"post_idem": OpPostIdem, "patch": OpPatch, "patch_idem": OpPatchIdem, "put": OpPut}
	for name, op := range ops {
		tr, err := in.ToolRequest(name, nil)
		if err != nil || tr.Op != op || string(tr.Body) != "{}" {
			t.Errorf("%s: %+v, %v", name, tr, err)
		}
	}
}

func TestToolRequestRejects(t *testing.T) {
	in := testIngress(t,
		ToolDef{Service: "s", Name: "get", Method: "GET", Path: "/i/{id}"},
	)
	cases := map[string]string{
		`[1]`:                    "JSON object",
		`"x"`:                    "JSON object",
		`{"id":`:                 "JSON object",
		`{}`:                     `"id" is required`,
		`{"id":""}`:              `"id" must not be empty`,
		`{"id":"."}`:             `"id" must not be empty`,
		`{"id":".."}`:            `"id" must not be empty`,
		`{"id":{"a":1}}`:         `"id" must be a string or number`,
		`{"id":true}`:            `"id" must be a string or number`,
		`{"id":"1","f":{"a":1}}`: `"f" must be a scalar`,
		`{"id":"1","f":[[1]]}`:   `"f" must be a scalar`,
	}
	for args, want := range cases {
		_, err := in.ToolRequest("get", json.RawMessage(args))
		var ae *apperr.Error
		if err == nil || !errors.As(err, &ae) || ae.Code != apperr.CodeInvalidRequest || !strings.Contains(ae.Message, want) {
			t.Errorf("ToolRequest(%s) error = %v, want %q", args, err, want)
		}
	}
	if _, err := in.ToolRequest("nope", nil); err == nil {
		t.Error("unknown tool accepted")
	}
}

func TestToolResult(t *testing.T) {
	in := Ingress{}
	h := func(ct string) http.Header {
		hd := http.Header{}
		if ct != "" {
			hd.Set("Content-Type", ct)
		}
		return hd
	}
	tests := []struct {
		name       string
		status     int
		ct, body   string
		wantText   string
		wantErr    bool
		structured bool
	}{
		{"json object", 200, "application/json; charset=utf-8", `{"a":1}`, `{"a":1}`, false, true},
		{"json suffix", 200, "application/problem+json", `{"a":1}`, `{"a":1}`, false, true},
		{"json array", 200, "application/json", `[1,2]`, `[1,2]`, false, false},
		{"invalid json", 200, "application/json", `{bad`, `{bad`, false, false},
		{"text", 200, "text/plain", "hello", "hello", false, false},
		{"no content type json", 200, "", ` {"a":1}`, ` {"a":1}`, false, true},
		{"empty", 204, "", "", "empty response (HTTP 204)", false, false},
		{"binary", 200, "application/pdf", "%PDF", "binary response (application/pdf, 4 bytes) is not supported by tools", true, false},
		{"invalid utf8 text", 200, "text/plain", "\xff\xfe", "binary response (text/plain, 2 bytes) is not supported by tools", true, false},
		{"upstream error", 404, "application/json", `{"error":"no"}`, `HTTP 404: {"error":"no"}`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := in.ToolResult("x", tt.status, h(tt.ct), []byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError != tt.wantErr || len(res.Content) != 1 || res.Content[0].Text != tt.wantText {
				t.Fatalf("result = %+v", res)
			}
			if (res.Structured != nil) != tt.structured {
				t.Errorf("structured = %s", res.Structured)
			}
		})
	}
}

func TestToolResultErrorTruncation(t *testing.T) {
	body := strings.Repeat("a", maxToolErrorBytes-1) + "中文" // 截断点落在多字节字符中间
	res, err := Ingress{}.ToolResult("x", 500, http.Header{}, []byte(body))
	if err != nil || !res.IsError {
		t.Fatalf("result = %+v, %v", res, err)
	}
	text := strings.TrimPrefix(res.Content[0].Text, "HTTP 500: ")
	if text != strings.Repeat("a", maxToolErrorBytes-1) {
		t.Errorf("truncated text has %d bytes, tail %q", len(text), text[len(text)-3:])
	}
	res, _ = Ingress{}.ToolResult("x", 500, http.Header{}, []byte("bad\xffbyte"))
	if res.Content[0].Text != "HTTP 500: bad�byte" {
		t.Errorf("invalid utf8 error text = %q", res.Content[0].Text)
	}
}

func TestRegisterWithOptionsTools(t *testing.T) {
	reg := protocol.NewRegistry()
	err := RegisterWithOptions(reg, Options{Tools: []ToolDef{
		{Service: "svc", Name: "svc_q", Description: "query things", Method: "POST", Path: "/q", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// 注册表封存需要全部适配器（含 Fallback），工具索引由 cmd/gateway 的端到端用例覆盖
	if err := RegisterWithOptions(protocol.NewRegistry(), Options{Tools: []ToolDef{{Name: "x"}}}); err == nil {
		t.Error("invalid tool def accepted")
	}
}

// FS-30：文件参数进 X-UAIG-File，name_query 进 X-UAIG-File-Name-Query，其余参数进查询串，请求体为空。
func TestFileToolRequest(t *testing.T) {
	in := testIngress(t,
		ToolDef{Service: "mu", Name: "parse", Description: "d", Method: "POST", Path: "/parse/file", InputSchema: schema, FileArg: "file", FileNameQuery: "name"},
		ToolDef{Service: "mu", Name: "put", Description: "d", Method: "PUT", Path: "/obj/{key}", InputSchema: schema, FileArg: "blob"},
		ToolDef{Service: "mu", Name: "idem", Description: "d", Method: "POST", Path: "/i", InputSchema: schema, FileArg: "f", Idempotent: true},
	)
	tr, err := in.ToolRequest("parse", json.RawMessage(`{"file":" file_abc ","is_ocr":true,"lang":"en"}`))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Op != OpPost || len(tr.Body) != 0 || tr.Header.Get(headerFile) != "file_abc" || tr.Header.Get(headerFileNameQuery) != "name" ||
		tr.Query.Encode() != "is_ocr=true&lang=en" || tr.PathValues["path"] != "parse/file" {
		t.Fatalf("parse = %+v", tr)
	}
	tr, err = in.ToolRequest("put", json.RawMessage(`{"blob":"file_x","key":"a b"}`))
	if err != nil || tr.Op != OpPut || tr.PathValues["path"] != "obj/a%20b" || tr.Header.Get(headerFileNameQuery) != "" || len(tr.Query) != 0 {
		t.Fatalf("put = %+v %v", tr, err)
	}
	if tr, err := in.ToolRequest("idem", json.RawMessage(`{"f":"file_x"}`)); err != nil || tr.Op != OpPostIdem {
		t.Fatalf("idem = %+v %v", tr, err)
	}
	for _, args := range []string{`{}`, `{"file":3}`, `{"file":""}`} {
		if _, err := in.ToolRequest("parse", json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), `argument "file" must be a file_id`) {
			t.Errorf("%s: %v", args, err)
		}
	}
	if _, err := in.ToolRequest("parse", json.RawMessage(`{"file":"file_x","nested":{"a":1}}`)); err == nil {
		t.Error("nested query argument accepted")
	}
}
