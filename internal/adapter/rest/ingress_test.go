package rest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestIngress_Routes(t *testing.T) {
	in := Ingress{}
	routes := in.Routes()
	if len(routes) != 7 {
		t.Fatalf("expected 7 routes, got %d", len(routes))
	}

	expected := map[string]struct {
		method string
		path   string
		body   protocol.BodyMode
	}{
		OpGet:       {http.MethodGet, "/rest/{service}/{path...}", protocol.BodyNone},
		OpDelete:    {http.MethodDelete, "/rest/{service}/{path...}", protocol.BodyNone},
		OpPut:       {http.MethodPut, "/rest/{service}/{path...}", protocol.BodyBuffered},
		OpPost:      {http.MethodPost, "/rest/{service}/{path...}", protocol.BodyStream},
		OpPatch:     {http.MethodPatch, "/rest/{service}/{path...}", protocol.BodyStream},
		OpPostIdem:  {http.MethodPost, "/rest-idem/{service}/{path...}", protocol.BodyBuffered},
		OpPatchIdem: {http.MethodPatch, "/rest-idem/{service}/{path...}", protocol.BodyBuffered},
	}

	seenOps := map[string]bool{}
	for _, r := range routes {
		exp, ok := expected[r.Op]
		if !ok {
			t.Errorf("unexpected op %q", r.Op)
			continue
		}
		if r.Path != exp.path {
			t.Errorf("route %s path = %q, want %q", r.Op, r.Path, exp.path)
		}
		if r.Kind != protocol.RouteCall {
			t.Errorf("route %s kind = %v, want RouteCall", r.Op, r.Kind)
		}
		if r.Method != exp.method {
			t.Errorf("route %s method = %s, want %s", r.Op, r.Method, exp.method)
		}
		if r.Body != exp.body {
			t.Errorf("route %s body mode = %v, want %v", r.Op, r.Body, exp.body)
		}
		if seenOps[r.Op] {
			t.Errorf("duplicate op %q (REG-02 violation)", r.Op)
		}
		seenOps[r.Op] = true
	}
}

func TestIngress_Parse_Valid(t *testing.T) {
	in := Ingress{}

	mux := http.NewServeMux()
	var capturedEnv *protocol.Envelope
	var captureErr error

	for _, rt := range in.Routes() {
		rRoute := rt
		mux.HandleFunc(rRoute.Method+" "+rRoute.Path, func(w http.ResponseWriter, req *http.Request) {
			capturedEnv, captureErr = in.Parse(req, rRoute, nil)
		})
	}

	cases := []struct {
		name       string
		method     string
		url        string
		wantOp     string
		wantModel  string
		wantMethod string
		wantPath   string
		wantQuery  string
	}{
		{
			name:       "get basic",
			method:     http.MethodGet,
			url:        "/rest/weather/forecast?city=Paris",
			wantOp:     OpGet,
			wantModel:  "weather",
			wantMethod: http.MethodGet,
			wantPath:   "/forecast",
			wantQuery:  "city=Paris",
		},
		{
			name:       "head request mapped to rest.get",
			method:     http.MethodHead,
			url:        "/rest/weather/forecast",
			wantOp:     OpGet,
			wantModel:  "weather",
			wantMethod: http.MethodHead,
			wantPath:   "/forecast",
			wantQuery:  "",
		},
		{
			name:       "empty subpath root",
			method:     http.MethodGet,
			url:        "/rest/weather/",
			wantOp:     OpGet,
			wantModel:  "weather",
			wantMethod: http.MethodGet,
			wantPath:   "/",
			wantQuery:  "",
		},
		{
			name:       "encoded path preserved",
			method:     http.MethodGet,
			url:        "/rest/storage/a%2Fb/c",
			wantOp:     OpGet,
			wantModel:  "storage",
			wantMethod: http.MethodGet,
			wantPath:   "/a%2Fb/c",
			wantQuery:  "",
		},
		{
			name:       "post with body stream",
			method:     http.MethodPost,
			url:        "/rest/notification/send?urgent=1",
			wantOp:     OpPost,
			wantModel:  "notification",
			wantMethod: http.MethodPost,
			wantPath:   "/send",
			wantQuery:  "urgent=1",
		},
		{
			name:       "delete request",
			method:     http.MethodDelete,
			url:        "/rest/storage/file.txt",
			wantOp:     OpDelete,
			wantModel:  "storage",
			wantMethod: http.MethodDelete,
			wantPath:   "/file.txt",
			wantQuery:  "",
		},
		{
			name:       "put request",
			method:     http.MethodPut,
			url:        "/rest/storage/file.txt",
			wantOp:     OpPut,
			wantModel:  "storage",
			wantMethod: http.MethodPut,
			wantPath:   "/file.txt",
			wantQuery:  "",
		},
		{
			name:       "patch request",
			method:     http.MethodPatch,
			url:        "/rest/users/42",
			wantOp:     OpPatch,
			wantModel:  "users",
			wantMethod: http.MethodPatch,
			wantPath:   "/42",
			wantQuery:  "",
		},
		{
			name:       "post idem request",
			method:     http.MethodPost,
			url:        "/rest-idem/bivia/boolean?trace=1",
			wantOp:     OpPostIdem,
			wantModel:  "bivia",
			wantMethod: http.MethodPost,
			wantPath:   "/boolean",
			wantQuery:  "trace=1",
		},
		{
			name:       "patch idem request",
			method:     http.MethodPatch,
			url:        "/rest-idem/users/42",
			wantOp:     OpPatchIdem,
			wantModel:  "users",
			wantMethod: http.MethodPatch,
			wantPath:   "/42",
			wantQuery:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturedEnv = nil
			captureErr = nil
			req := httptest.NewRequest(tc.method, tc.url, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if captureErr != nil {
				t.Fatalf("unexpected parse error: %v", captureErr)
			}
			if capturedEnv == nil {
				t.Fatalf("request was not routed or envelope is nil")
			}
			if capturedEnv.Op != tc.wantOp {
				t.Errorf("Op = %q, want %q", capturedEnv.Op, tc.wantOp)
			}
			if capturedEnv.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", capturedEnv.Model, tc.wantModel)
			}
			if capturedEnv.Params["method"] != tc.wantMethod {
				t.Errorf("Params[method] = %q, want %q", capturedEnv.Params["method"], tc.wantMethod)
			}
			if capturedEnv.Params["path"] != tc.wantPath {
				t.Errorf("Params[path] = %q, want %q", capturedEnv.Params["path"], tc.wantPath)
			}
			if capturedEnv.Params["query"] != tc.wantQuery {
				t.Errorf("Params[query] = %q, want %q", capturedEnv.Params["query"], tc.wantQuery)
			}
			if capturedEnv.Estimate[UnitRequests] != 1 {
				t.Errorf("Estimate[requests] = %d, want 1", capturedEnv.Estimate[UnitRequests])
			}
		})
	}
}

func TestIngress_Parse_PathSafety(t *testing.T) {
	in := Ingress{}
	rt := protocol.Route{Method: http.MethodGet, Path: "/rest/{service}/{path...}", Op: OpGet}

	// 1. 通过 ServeMux 路由测试百分号编码的各种穿越路径
	mux := http.NewServeMux()
	var captureErr error
	mux.HandleFunc("GET /rest/{service}/{path...}", func(w http.ResponseWriter, req *http.Request) {
		_, captureErr = in.Parse(req, rt, nil)
	})

	encodedPaths := []string{
		"/rest/svc/%2e%2e",
		"/rest/svc/%2e%2e/file",
		"/rest/svc/%2E.",
		"/rest/svc/%2E./file",
		"/rest/svc/.%2e",
		"/rest/svc/%2e",
		"/rest/svc/foo%5cbar",
		"/rest/svc/foo%00bar",
	}

	for _, p := range encodedPaths {
		t.Run(p, func(t *testing.T) {
			captureErr = nil
			req := httptest.NewRequest(http.MethodGet, p, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if captureErr == nil {
				t.Fatalf("expected error for path %q, got nil", p)
			}
			appErr, ok := apperr.As(captureErr)
			if !ok {
				t.Fatalf("expected *apperr.Error, got %v", captureErr)
			}
			if appErr.Code != apperr.CodeInvalidRequest {
				t.Errorf("expected CodeInvalidRequest, got %v", appErr.Code)
			}
			if appErr.Message != "path must not contain dot segments" {
				t.Errorf("expected message 'path must not contain dot segments', got %q", appErr.Message)
			}
		})
	}

	// 2. 直接调用 Parse 测试未编码的点段或反斜杠段（绕过 ServeMux 的前置路径清理）
	rawPaths := []string{
		"/..",
		"/../etc/passwd",
		"/./config",
		"/sub/..",
		"/foo\\bar",
	}

	for _, p := range rawPaths {
		t.Run("raw "+p, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://gateway/rest/svc"+p, nil)
			req.SetPathValue("service", "svc")
			// 模拟未经 clean 的原始请求 URL
			req.URL.Path = "/rest/svc" + p
			req.URL.RawPath = "/rest/svc" + p

			_, err := in.Parse(req, rt, nil)
			if err == nil {
				t.Fatalf("expected error for raw path %q, got nil", p)
			}
			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("expected *apperr.Error, got %v", err)
			}
			if appErr.Code != apperr.CodeInvalidRequest {
				t.Errorf("expected CodeInvalidRequest, got %v", appErr.Code)
			}
			if appErr.Message != "path must not contain dot segments" {
				t.Errorf("expected message 'path must not contain dot segments', got %q", appErr.Message)
			}
		})
	}
}

func TestIngress_ErrorsAndCatalog(t *testing.T) {
	in := Ingress{}

	// WriteError
	w := httptest.NewRecorder()
	testErr := apperr.New(apperr.CodeInvalidRequest, "test message")
	in.WriteError(w, "req-123", testErr)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	var errResp map[string]map[string]string
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode JSON error: %v", err)
	}
	if errResp["error"]["code"] != string(apperr.CodeInvalidRequest) {
		t.Errorf("error.code = %q, want %q", errResp["error"]["code"], apperr.CodeInvalidRequest)
	}
	if errResp["error"]["message"] != "test message" {
		t.Errorf("error.message = %q, want 'test message'", errResp["error"]["message"])
	}

	// StreamError
	ev := in.StreamError(testErr)
	if !bytes.Contains(ev.Data, []byte("test message")) {
		t.Errorf("StreamError data does not contain 'test message': %s", string(ev.Data))
	}

	// WriteCatalog
	wCatalog := httptest.NewRecorder()
	in.WriteCatalog(wCatalog, nil)
	if wCatalog.Code != http.StatusNotFound {
		t.Errorf("WriteCatalog status = %d, want 404", wCatalog.Code)
	}
}

func TestIngress_RestIdem_OtherMethodsNotDeclared(t *testing.T) {
	in := Ingress{}
	for _, rt := range in.Routes() {
		if strings.HasPrefix(rt.Path, "/rest-idem/") {
			if rt.Method != http.MethodPost && rt.Method != http.MethodPatch {
				t.Errorf("unexpected method %s declared for /rest-idem/", rt.Method)
			}
		}
	}
}
