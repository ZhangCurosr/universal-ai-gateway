package mineru

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestValidFileName(t *testing.T) {
	for name, want := range map[string]bool{
		"report.pdf": true, "报告 2026.docx": true, "a.b.png": true,
		"": false, "noext": false, ".pdf": false, "x.": false, "../a.pdf": false, `dir\a.pdf`: false,
		"a\x00.pdf": false, "a\n.pdf": false, strings.Repeat("a", 252) + ".pdf": false,
	} {
		if got := validFileName(name); got != want {
			t.Errorf("validFileName(%q) = %v, want %v", name, got, want)
		}
	}
}

// MU-07：查询参数 → 申请上传地址的请求体。
func TestUploadBody(t *testing.T) {
	b, err := uploadBody(map[string]string{
		"name": "a.pdf", "size": "9", "is_ocr": "true", "enable_formula": "false", "enable_table": "1",
		"language": "en", "model_version": "vlm", "data_id": "d-1", "page_ranges": "1-3", "extra_formats": "docx, html,",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := `{"enable_formula":false,"enable_table":true,"extra_formats":["docx","html"],"files":[{"data_id":"d-1","is_ocr":true,"name":"a.pdf","page_ranges":"1-3"}],"language":"en","model_version":"vlm"}`
	if string(b) != want {
		t.Errorf("uploadBody =\n%s\nwant\n%s", b, want)
	}
	if b, _ := uploadBody(map[string]string{"name": "a.pdf", "size": "1"}); string(b) != `{"files":[{"name":"a.pdf"}]}` {
		t.Errorf("minimal body = %s", b)
	}
	for name, p := range map[string]map[string]string{
		"no name":     {"size": "1"},
		"bad name":    {"name": "x", "size": "1"},
		"no size":     {"name": "a.pdf"},
		"zero size":   {"name": "a.pdf", "size": "0"},
		"bad boolean": {"name": "a.pdf", "size": "1", "enable_table": "yes please"},
	} {
		if _, err := uploadBody(p); apperr.CodeOf(err) != apperr.CodeInvalidRequest {
			t.Errorf("%s: err = %v, want invalid_request", name, err)
		}
	}
}

// uploadStub 模拟申请地址的上游；file_urls 指向 target（由测试决定是否同源）。
func uploadStub(t *testing.T, apply string, batch func(w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	var puts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/file-urls/batch":
			_, _ = io.WriteString(w, strings.ReplaceAll(apply, "{self}", "http://"+r.Host))
		case r.Method == http.MethodPut:
			atomic.AddInt32(&puts, 1)
			_, _ = io.Copy(io.Discard, r.Body)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/extract-results/batch/"):
			batch(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &puts
}

func uploadCall(op string) *protocol.Call {
	return &protocol.Call{Op: op, Body: []byte(`{"files":[{"name":"a.pdf"}]}`), BodyStream: strings.NewReader("12345"),
		Params: map[string]string{"name": "a.pdf", "size": "5"}, State: pricing.Usage{UnitPages: 2}}
}

// MU-07：第三方存储（签名地址）不带上游 Key，定长上传，不设 Content-Type。
func TestUpload_ForeignStorageGetsNoKey(t *testing.T) {
	var auth, ctype string
	var length int64
	var got []byte
	oss := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ctype, length = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.ContentLength
		got, _ = io.ReadAll(r.Body)
	}))
	defer oss.Close()
	srv, _ := uploadStub(t, `{"code":0,"data":{"batch_id":"b9","file_urls":["`+oss.URL+`/bucket/a.pdf?Signature=x"]}}`, nil)
	up := protocol.Upstream{BaseURL: srv.URL, Secret: []byte("sk-upstream"), Client: oss.Client(), MaxBufferedBytes: 1 << 20}
	resp, err := fastPoll.Do(context.Background(), up, uploadCall(OpTaskUpload))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" || ctype != "" || length != 5 || string(got) != "12345" {
		t.Errorf("storage got auth=%q ctype=%q len=%d body=%q", auth, ctype, length, got)
	}
	if len(resp.Pins) != 1 || resp.Pins[0] != "batch-b9" || !strings.Contains(string(resp.Body), `"task_id":"batch-b9"`) {
		t.Errorf("response = %s pins=%v", resp.Body, resp.Pins)
	}
	if u, src := resp.Meter.Usage(); src != protocol.UsageEstimated || u[UnitPages] != 2 {
		t.Errorf("usage = %v (%v)", u, src)
	}
}

// 上传完成之前的失败：上游不会创建任务，不计费。
func TestUpload_FailuresBeforeUploadAreFree(t *testing.T) {
	for name, tt := range map[string]struct {
		apply    string
		putCode  int
		wantKind protocol.OutcomeKind
		wantPuts int32
	}{
		"token error":        {apply: `{"code":"A0202","msg":"bad token"}`, wantKind: protocol.OutcomeAuthFailed},
		"daily quota":        {apply: `{"code":-60018,"msg":"quota"}`, wantKind: protocol.OutcomeRateLimited},
		"no batch id":        {apply: `{"code":0,"data":{"file_urls":["{self}/x"]}}`, wantKind: protocol.OutcomeServerError},
		"foreign plain http": {apply: `{"code":0,"data":{"batch_id":"b","file_urls":["http://198.51.100.7/x"]}}`, wantKind: protocol.OutcomeServerError},
		"storage rejects":    {apply: `{"code":0,"data":{"batch_id":"b","file_urls":["{self}/put"]}}`, putCode: 403, wantKind: protocol.OutcomePermissionDenied, wantPuts: 1},
	} {
		t.Run(name, func(t *testing.T) {
			var puts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					atomic.AddInt32(&puts, 1)
					w.WriteHeader(tt.putCode)
					return
				}
				_, _ = io.WriteString(w, strings.ReplaceAll(tt.apply, "{self}", "http://"+r.Host))
			}))
			defer srv.Close()
			resp, err := fastPoll.Do(context.Background(), protocol.Upstream{BaseURL: srv.URL, Secret: []byte("k"), Client: srv.Client()}, uploadCall(OpParseFile))
			var ae *protocol.AttemptError
			if !errors.As(err, &ae) || ae.Outcome.Kind != tt.wantKind {
				t.Fatalf("err = %v, want %v", err, tt.wantKind)
			}
			if resp != nil {
				t.Error("failure before the upload completed returned a billable response")
			}
			if puts != tt.wantPuts {
				t.Errorf("PUT requests = %d, want %d", puts, tt.wantPuts)
			}
		})
	}
}

// MU-08：batch- 任务的查询与下载；批次中的失败按 MU-03 返回。
func TestUpload_BatchQueries(t *testing.T) {
	state := `[]`
	srv, _ := uploadStub(t, "", func(w http.ResponseWriter) {
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"batch_id":"b7","extract_result":%s}}`, state)
	})
	up := protocol.Upstream{BaseURL: srv.URL, Secret: []byte("k"), Client: srv.Client(), MaxBufferedBytes: 1 << 20}
	get := func() string {
		t.Helper()
		resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpTaskGet, Params: map[string]string{"id": "batch-b7"}})
		if err != nil {
			t.Fatal(err)
		}
		return string(resp.Body)
	}
	if got := get(); got != `{"code":0,"data":{"batch_id":"b7","state":"waiting-file","task_id":"batch-b7"},"msg":"ok"}` {
		t.Errorf("empty batch = %s", got)
	}
	state = `[{"file_name":"a.pdf","state":"Running","err_msg":"","data_id":"d1","extract_progress":{"extracted_pages":1,"total_pages":3}}]`
	if got := get(); !strings.Contains(got, `"state":"running"`) || !strings.Contains(got, `"total_pages":3`) || !strings.Contains(got, `"data_id":"d1"`) || !strings.Contains(got, `"file_name":"a.pdf"`) {
		t.Errorf("running batch = %s", got)
	}
	_, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpTaskResult, Params: map[string]string{"id": "batch-b7"}})
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Status != http.StatusConflict {
		t.Errorf("result while running: %v", err)
	}

	// 同步上传：批次失败 → 400，带原因；上传后的失败保留计量
	state = `[{"file_name":"a.pdf","state":"failed","err_msg":"unsupported file type"}]`
	srv2, _ := uploadStub(t, `{"code":0,"data":{"batch_id":"b7","file_urls":["{self}/put"]}}`, func(w http.ResponseWriter) {
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"batch_id":"b7","extract_result":%s}}`, state)
	})
	resp, err := fastPoll.Do(context.Background(), protocol.Upstream{BaseURL: srv2.URL, Secret: []byte("k"), Client: srv2.Client(), MaxBufferedBytes: 1 << 20}, uploadCall(OpParseFile))
	if !errors.As(err, &ae) || ae.Outcome.Status != http.StatusBadRequest || !strings.Contains(string(ae.Body), "unsupported file type") {
		t.Fatalf("failed batch: %v", err)
	}
	if resp == nil || resp.Meter == nil {
		t.Error("failure after upload must keep usage (CORE-12)")
	}
}
