package files

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

func TestAttach(t *testing.T) {
	s, _, _ := newStore(t)
	m := put(t, s, 1, "file body")

	r := httptest.NewRequest(http.MethodPost, "/rest/x/parse?lang=en", nil)
	r.Header.Set(HeaderFile, m.ID)
	r.Header.Set(HeaderFileNameQuery, "name")
	cleanup, err := Attach(s, r, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	cleanup()
	if string(b) != "file body" || r.ContentLength != 9 || r.Header.Get("Content-Type") != "application/pdf" ||
		r.URL.RawQuery != "lang=en&name=a.pdf" || r.Header.Get(HeaderFile) != "" || r.Header.Get(HeaderFileNameQuery) != "" {
		t.Fatalf("body=%q len=%d ct=%q query=%q", b, r.ContentLength, r.Header.Get("Content-Type"), r.URL.RawQuery)
	}

	// 客户端已给出的参数与 Content-Type 保留
	r = httptest.NewRequest(http.MethodPost, "/x?name=mine.pdf", nil)
	r.Header.Set(HeaderFile, m.ID)
	r.Header.Set(HeaderFileNameQuery, "name")
	r.Header.Set("Content-Type", "text/plain")
	cleanup, err = Attach(s, r, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if r.URL.RawQuery != "name=mine.pdf" || r.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("query=%q ct=%q", r.URL.RawQuery, r.Header.Get("Content-Type"))
	}
	if s.files[m.ID].readers != 0 {
		t.Fatal("reader not released")
	}
}

func TestAttachRejects(t *testing.T) {
	s, _, _ := newStore(t)
	m := put(t, s, 1, "x")
	cases := []struct {
		name      string
		store     *Store
		owner     int64
		allowBody bool
		body      string
		nameQuery string
		want      apperr.Code
		msg       string
	}{
		{"disabled", nil, 1, true, "", "", apperr.CodeInvalidRequest, "files.enabled"},
		{"no body route", s, 1, false, "", "", apperr.CodeInvalidRequest, "without a request body"},
		{"own body", s, 1, true, "data", "", apperr.CodeInvalidRequest, "must be empty"},
		{"bad name query", s, 1, true, "", "a b", apperr.CodeInvalidRequest, HeaderFileNameQuery},
		{"other owner", s, 2, true, "", "", apperr.CodeNotFound, "not found"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(c.body))
		r.Header.Set(HeaderFile, m.ID)
		if c.nameQuery != "" {
			r.Header.Set(HeaderFileNameQuery, c.nameQuery)
		}
		cleanup, err := Attach(c.store, r, c.owner, c.allowBody)
		cleanup()
		if code(err) != c.want || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// 没有 X-UAIG-File：只删除名称参数头，请求不变
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("keep"))
	r.Header.Set(HeaderFileNameQuery, "name")
	cleanup, err := Attach(nil, r, 1, true)
	cleanup()
	b, _ := io.ReadAll(r.Body)
	if err != nil || string(b) != "keep" || r.Header.Get(HeaderFileNameQuery) != "" || Wants(r) {
		t.Fatalf("passthrough: %v %q", err, b)
	}
}
