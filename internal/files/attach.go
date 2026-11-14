package files

import (
	"io"
	"net/http"
	"net/url"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// 用文件 ID 代替请求体的请求头（FS-20、FS-21）。
const (
	HeaderFile          = "X-UAIG-File"
	HeaderFileNameQuery = "X-UAIG-File-Name-Query"
)

// Wants 报告请求是否要用文件代替请求体。
func Wants(r *http.Request) bool { return r.Header.Get(HeaderFile) != "" }

// Attach 实现 FS-20 / FS-21：请求带 X-UAIG-File 时，把请求体替换为 owner 的该文件，
// Content-Length 设为文件大小，没有 Content-Type 时用文件的类型；按 X-UAIG-File-Name-Query 补上文件名查询参数。
// 两个请求头总是被删除。返回的 cleanup 必须在请求处理结束后调用（关闭文件）。
// s 为 nil（没有开启文件中转）而请求带了 X-UAIG-File → 400。allowBody 为 false（BodyNone 路由）时带这个头 → 400。
func Attach(s *Store, r *http.Request, owner int64, allowBody bool) (cleanup func(), err error) {
	id := r.Header.Get(HeaderFile)
	nameQuery := r.Header.Get(HeaderFileNameQuery)
	r.Header.Del(HeaderFile)
	r.Header.Del(HeaderFileNameQuery)
	noop := func() {}
	if id == "" {
		return noop, nil
	}
	switch {
	case s == nil:
		return noop, apperr.New(apperr.CodeInvalidRequest, HeaderFile+" needs files.enabled on the gateway")
	case !allowBody:
		return noop, apperr.New(apperr.CodeInvalidRequest, HeaderFile+" is not allowed on an endpoint without a request body")
	case nameQuery != "" && !nameQueryPattern.MatchString(nameQuery):
		return noop, apperr.New(apperr.CodeInvalidRequest, HeaderFileNameQuery+" must match ^[A-Za-z0-9_.-]{1,64}$")
	}
	if r.Body != nil && r.Body != http.NoBody {
		var b [1]byte
		if n, _ := io.ReadFull(r.Body, b[:]); n > 0 { // 只判断是否为空；读取错误等同于空
			return noop, apperr.New(apperr.CodeInvalidRequest, "the request body must be empty when "+HeaderFile+" is set")
		}
	}
	f, err := s.OpenFile(owner, id)
	if err != nil {
		return noop, err
	}
	r.Body = f
	r.ContentLength = f.Size
	r.Header.Del("Content-Length")
	r.Header.Del("Transfer-Encoding")
	r.TransferEncoding = nil
	if r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", f.ContentType)
	}
	if nameQuery != "" {
		q := r.URL.Query()
		if _, ok := q[nameQuery]; !ok {
			extra := url.QueryEscape(nameQuery) + "=" + url.QueryEscape(f.Name)
			if r.URL.RawQuery == "" {
				r.URL.RawQuery = extra
			} else {
				r.URL.RawQuery += "&" + extra
			}
		}
	}
	return func() { _ = f.Close() /* 只读文件，关闭失败无影响 */ }, nil
}
