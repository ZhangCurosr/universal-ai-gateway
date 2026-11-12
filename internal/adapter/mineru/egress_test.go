package mineru

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// resultZip 生成 MinerU 结果包：full.md，pages > 0 时附 layout.json（pdf_info 长度 = 页数）。
func resultZip(t *testing.T, dir, md string, pages int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(dir + "full.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, md); err != nil {
		t.Fatal(err)
	}
	if pages > 0 {
		w, err := zw.Create(dir + "layout.json")
		if err != nil {
			t.Fatal(err)
		}
		info := make([]map[string]int, pages)
		if err := json.NewEncoder(w).Encode(map[string]any{"pdf_info": info}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const okSubmit = `{"code":0,"msg":"ok","trace_id":"t","data":{"task_id":"task_1"}}`

// v4Stub 模拟 MinerU v4：提交返回 task_1，查询交给 poll（参数 zipURL 为结果包地址），结果包为 zipBody。
func v4Stub(t *testing.T, poll func(w http.ResponseWriter, zipURL string), zipBody []byte) (*httptest.Server, protocol.Upstream, *http.Header) {
	t.Helper()
	zipHeaders := &http.Header{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/extract/task":
			_, _ = io.WriteString(w, okSubmit)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/extract/task/task_1":
			poll(w, srv.URL+"/files/task_1.zip")
		case r.Method == http.MethodGet && r.URL.Path == "/files/task_1.zip":
			*zipHeaders = r.Header.Clone()
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, protocol.Upstream{BaseURL: srv.URL, Secret: []byte("test_secret"), Client: srv.Client(), MaxBufferedBytes: 1 << 20}, zipHeaders
}

func doneState(zipURL string, pages int) string {
	progress := ""
	if pages > 0 {
		progress = fmt.Sprintf(`,"extract_progress":{"extracted_pages":%d,"total_pages":%d}`, pages, pages)
	}
	return fmt.Sprintf(`{"code":0,"msg":"ok","data":{"task_id":"task_1","state":"done","full_zip_url":%q,"err_msg":""%s}}`, zipURL, progress)
}

var fastPoll = Egress{InitialPollInterval: time.Millisecond, MaxPollInterval: 2 * time.Millisecond}

var urlBody = []byte(`{"url":"https://example.com/doc.pdf"}`)

func TestEgressUnits(t *testing.T) {
	units := Egress{}.Units()
	if len(units) != 2 || units[0] != UnitPages || units[1] != UnitRequests {
		t.Errorf("Units() = %v, want [%q, %q]", units, UnitPages, UnitRequests)
	}
}

// MU-02：提交类请求体必须是带 url 的 JSON 对象；查询参数 url 在请求体没有 url 时补入。
func TestEgressPrepare_Validation(t *testing.T) {
	eg := Egress{}
	call, err := eg.Prepare(&protocol.Envelope{Op: OpParse, Model: ModelParse, Body: []byte(`{"url":"https://example.com/doc.pdf","is_ocr":true}`)}, "upstream-mineru")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if call.Model != "upstream-mineru" || !strings.Contains(string(call.Body), `"is_ocr":true`) {
		t.Errorf("call = %+v", call)
	}

	call, err = eg.Prepare(&protocol.Envelope{Op: OpTaskCreate, Params: map[string]string{"url": "https://example.com/q.pdf"}}, "")
	if err != nil || string(call.Body) != `{"url":"https://example.com/q.pdf"}` {
		t.Errorf("query url: body=%s err=%v", call.Body, err)
	}
	call, err = eg.Prepare(&protocol.Envelope{Op: OpTaskCreate, Body: urlBody, Params: map[string]string{"url": "https://other/x.pdf"}}, "")
	if err != nil || !strings.Contains(string(call.Body), "example.com/doc.pdf") {
		t.Errorf("body url must win: body=%s err=%v", call.Body, err)
	}

	for name, env := range map[string]*protocol.Envelope{
		"unsupported op":     {Op: "unsupported"},
		"get without id":     {Op: OpTaskGet, Params: map[string]string{}},
		"result blank id":    {Op: OpTaskResult, Params: map[string]string{"id": "   "}},
		"array body":         {Op: OpParse, Body: []byte(`[1, 2, 3]`)},
		"binary body":        {Op: OpParse, Body: []byte("%PDF-1.7 ...")},
		"missing url":        {Op: OpParse, Body: []byte(`{"is_ocr":true}`)},
		"empty url":          {Op: OpTaskCreate, Body: []byte(`{"url":"  "}`)},
		"non-string url":     {Op: OpTaskCreate, Body: []byte(`{"url":5}`)},
		"bad json":           {Op: OpTaskCreate, Body: []byte(`{"url":`)},
		"no body, no url qs": {Op: OpParse},
	} {
		if _, err := eg.Prepare(env, ""); apperr.CodeOf(err) != apperr.CodeInvalidRequest {
			t.Errorf("%s: error = %v, want invalid_request", name, err)
		}
	}
}

func TestEgressDo_UnsupportedOp(t *testing.T) {
	_, err := Egress{}.Do(context.Background(), protocol.Upstream{}, &protocol.Call{Op: "invalid"})
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("unsupported op error = %v, want invalid_request", err)
	}
}

// MU-03：同步封装返回 full.md，按 extract_progress.total_pages 计费；结果包下载不带上游 Key。
func TestEgressDo_ParseReturnsMarkdown(t *testing.T) {
	polls := 0
	_, up, zipHeaders := v4Stub(t, func(w http.ResponseWriter, zipURL string) {
		polls++
		switch polls {
		case 1:
			_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"task_1","state":"pending"}}`)
		case 2:
			_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"task_1","state":"running","extract_progress":{"extracted_pages":1,"total_pages":7}}}`)
		default:
			_, _ = io.WriteString(w, doneState(zipURL, 0))
		}
	}, resultZip(t, "abc/", "# Title\n\nbody", 0))
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Body: urlBody})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if string(resp.Body) != "# Title\n\nbody" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Errorf("response = %q %v", resp.Body, resp.Header)
	}
	if u, src := resp.Meter.Usage(); src != protocol.UsageUpstream || u[UnitPages] != 7 || u[UnitRequests] != 1 {
		t.Errorf("usage = %v (%v), want pages=7 upstream", u, src)
	}
	if zipHeaders.Get("Authorization") != "Bearer test_secret" {
		t.Error("same-origin result download should carry the upstream key")
	}
}

// MU-05：第三方 CDN 上的结果包不带上游 Key。
func TestEgressDo_ZipFromCDNHasNoKey(t *testing.T) {
	var cdnAuth string
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = r.Header.Get("Authorization")
		_, _ = w.Write(resultZip(t, "", "# cdn", 2))
	}))
	defer cdn.Close()
	_, up, _ := v4Stub(t, func(w http.ResponseWriter, _ string) {
		_, _ = io.WriteString(w, doneState(cdn.URL+"/r.zip", 0))
	}, nil)
	up.Client = cdn.Client() // 信任 CDN 的测试证书；同样能访问 http 的上游
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Body: urlBody})
	if err != nil || string(resp.Body) != "# cdn" {
		t.Fatalf("Do = %v", err)
	}
	if cdnAuth != "" {
		t.Errorf("CDN received Authorization %q", cdnAuth)
	}
}

func TestEgressDo_PageCountFromLayout(t *testing.T) {
	_, up, _ := v4Stub(t, func(w http.ResponseWriter, zipURL string) {
		_, _ = io.WriteString(w, doneState(zipURL, 0))
	}, resultZip(t, "", "# doc", 4))
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Body: urlBody})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if u, src := resp.Meter.Usage(); src != protocol.UsageUpstream || u[UnitPages] != 4 {
		t.Errorf("usage = %v (%v), want pages=4 from layout.json", u, src)
	}
}

// spec/08 §8.2 + spec/06 §3.3：页数未知时不能按 0 页结算，交给内核按预留结算。
func TestEgressDo_NoPageCountFallsBackToReserve(t *testing.T) {
	_, up, _ := v4Stub(t, func(w http.ResponseWriter, zipURL string) {
		_, _ = io.WriteString(w, doneState(zipURL, 0))
	}, resultZip(t, "", "# doc", 0))
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Body: urlBody})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if _, src := resp.Meter.Usage(); src != protocol.UsageNone {
		t.Errorf("usage source = %v, want UsageNone", src)
	}
}

func TestEgressDo_ContextCanceledDuringPoll(t *testing.T) {
	_, up, _ := v4Stub(t, func(w http.ResponseWriter, _ string) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"state":"running"}}`)
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	start := time.Now()
	resp, err := Egress{}.Do(ctx, up, &protocol.Call{Op: OpParse, Body: urlBody, State: pricing.Usage{UnitPages: 3}})
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Do took %v", time.Since(start))
	}
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || (ae.Outcome.Kind != protocol.OutcomeCanceled && ae.Outcome.Kind != protocol.OutcomeTimeout) {
		t.Fatalf("Do error = %v", err)
	}
	if resp == nil || resp.Meter == nil {
		t.Fatal("canceled after submit must still report usage (CORE-12)")
	}
}

func TestEgressDo_TaskFailedStatusMapped(t *testing.T) {
	_, up, _ := v4Stub(t, func(w http.ResponseWriter, _ string) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"task_1","state":"failed","err_msg":"file corrupted or unparseable"}}`)
	}, nil)
	_, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Body: urlBody})
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) {
		t.Fatalf("Do error = %v, want *protocol.AttemptError", err)
	}
	if ae.Outcome.Kind != protocol.OutcomeClientError || ae.Outcome.Status != http.StatusBadRequest {
		t.Errorf("Outcome = %+v, want client error 400", ae.Outcome)
	}
	if !strings.Contains(string(ae.Body), "file corrupted") || !strings.Contains(ae.Err.Error(), "file corrupted") {
		t.Errorf("error = %v body = %s", ae.Err, ae.Body)
	}
}

// spec/08 §8.2：任务提交后轮询失败不得重试，否则会产生第二个付费任务。
func TestEgressDo_FailureAfterSubmitNotRetryable(t *testing.T) {
	for name, poll := range map[string]func(w http.ResponseWriter, zipURL string){
		"poll 503":        func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusServiceUnavailable) },
		"poll token lost": func(w http.ResponseWriter, _ string) { _, _ = io.WriteString(w, `{"code":"A0211","msg":"expired"}`) },
		"zip broken":      func(w http.ResponseWriter, zipURL string) { _, _ = io.WriteString(w, doneState(zipURL, 2)) },
	} {
		t.Run(name, func(t *testing.T) {
			_, up, _ := v4Stub(t, poll, []byte("not a zip"))
			resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpParse, Body: urlBody})
			var ae *protocol.AttemptError
			if !errors.As(err, &ae) {
				t.Fatalf("Do() error = %v, want *protocol.AttemptError", err)
			}
			if ae.Outcome.Kind.Retryable() {
				t.Errorf("Outcome.Kind = %v, want non-retryable", ae.Outcome.Kind)
			}
			if !strings.Contains(err.Error(), "task_1") {
				t.Errorf("error %q should carry the task id", err)
			}
			if resp == nil || resp.Meter == nil {
				t.Error("failure after submit must keep usage")
			}
		})
	}
}

// MU-04：业务码分类；提交被拒时不计费。
func TestEgressDo_BusinessCodes(t *testing.T) {
	now := time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC) // 北京时间 23:30
	for _, tt := range []struct {
		code   string
		kind   protocol.OutcomeKind
		status int
	}{
		{`"A0202"`, protocol.OutcomeAuthFailed, 401},
		{`"A0211"`, protocol.OutcomeAuthFailed, 401},
		{`-60018`, protocol.OutcomeRateLimited, 429},
		{`-60019`, protocol.OutcomeRateLimited, 429},
		{`-60009`, protocol.OutcomeOverloaded, 503},
		{`-60007`, protocol.OutcomeOverloaded, 503},
		{`-10001`, protocol.OutcomeOverloaded, 503},
		{`-60012`, protocol.OutcomeClientError, 404},
		{`-60013`, protocol.OutcomeClientError, 404},
		{`-60008`, protocol.OutcomeClientError, 400},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"code":%s,"msg":"boom"}`, tt.code)
		}))
		eg := Egress{Now: func() time.Time { return now }}
		resp, err := eg.Do(context.Background(), protocol.Upstream{BaseURL: srv.URL, Secret: []byte("k"), Client: srv.Client()},
			&protocol.Call{Op: OpTaskCreate, Body: urlBody, State: pricing.Usage{UnitPages: 5}})
		srv.Close()
		var ae *protocol.AttemptError
		if !errors.As(err, &ae) || ae.Outcome.Kind != tt.kind || ae.Outcome.Status != tt.status {
			t.Errorf("code %s: err = %v", tt.code, err)
			continue
		}
		if resp != nil {
			t.Errorf("code %s: rejected submission returned a billable response", tt.code)
		}
		if !strings.Contains(string(ae.Body), "boom") {
			t.Errorf("code %s: body = %s", tt.code, ae.Body)
		}
		if tt.kind == protocol.OutcomeRateLimited && ae.Outcome.RetryAfter != 31*time.Minute {
			t.Errorf("code %s: RetryAfter = %v, want 31m (until 00:01 UTC+8)", tt.code, ae.Outcome.RetryAfter)
		}
	}
}

func TestEgressDo_TaskCreatePinsAndAuth(t *testing.T) {
	var auth, ctype string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/api/v4/extract/task" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"task_created_101"}}`)
	}))
	defer srv.Close()
	resp, err := Egress{}.Do(context.Background(), protocol.Upstream{BaseURL: srv.URL, Secret: []byte("upstream-secret-key-xyz"), Client: srv.Client(), MaxBufferedBytes: 1 << 20},
		&protocol.Call{Op: OpTaskCreate, Body: urlBody, Header: http.Header{"Authorization": {"Bearer client-token-must-not-leak"}}, State: pricing.Usage{UnitPages: 5, UnitRequests: 1}})
	if err != nil {
		t.Fatalf("Do error = %v", err)
	}
	if auth != "Bearer upstream-secret-key-xyz" || ctype != "application/json" || string(body) != string(urlBody) {
		t.Errorf("upstream got auth=%q ctype=%q body=%s", auth, ctype, body)
	}
	if len(resp.Pins) != 1 || resp.Pins[0] != "task_created_101" {
		t.Errorf("Pins = %v", resp.Pins)
	}
	if u, src := resp.Meter.Usage(); src != protocol.UsageEstimated || u[UnitPages] != 5 || u[UnitRequests] != 1 {
		t.Errorf("usage = %v (%v)", u, src)
	}
}

// MU-05：下载结果——完成时转发压缩包，未完成 409，找不到任务 404。
func TestEgressDo_TaskResult(t *testing.T) {
	zipBytes := resultZip(t, "", "# r", 1)
	state := "running"
	_, up, zipHeaders := v4Stub(t, func(w http.ResponseWriter, zipURL string) {
		if state == "done" {
			_, _ = io.WriteString(w, doneState(zipURL, 1))
			return
		}
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"task_id":"task_1","state":%q}}`, state)
	}, zipBytes)

	_, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpTaskResult, Params: map[string]string{"id": "task_1"}})
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeClientError || ae.Outcome.Status != http.StatusConflict || !strings.Contains(string(ae.Body), "running") {
		t.Fatalf("unfinished result: %v", err)
	}

	state = "done"
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpTaskResult, Params: map[string]string{"id": "task_1"}})
	if err != nil {
		t.Fatalf("Do error = %v", err)
	}
	got, _ := io.ReadAll(resp.Raw)
	_ = resp.Raw.Close() // 测试清理
	if resp.Kind != protocol.ResponseRaw || !bytes.Equal(got, zipBytes) || resp.Header.Get("Content-Disposition") != `attachment; filename="task_1.zip"` {
		t.Errorf("result = kind %v, %d bytes, %v", resp.Kind, len(got), resp.Header)
	}
	if zipHeaders.Get("Authorization") != "Bearer test_secret" {
		t.Error("same-origin result download should carry the upstream key")
	}

	_, err = fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpTaskGet, Params: map[string]string{"id": "missing"}})
	if !errors.As(err, &ae) || ae.Outcome.Status != http.StatusNotFound {
		t.Errorf("unknown task: %v", err)
	}
}

func TestEgressDo_TaskGetIsFree(t *testing.T) {
	_, up, _ := v4Stub(t, func(w http.ResponseWriter, _ string) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"task_1","state":"pending"}}`)
	}, nil)
	resp, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpTaskGet, Params: map[string]string{"id": "task_1"}})
	if err != nil || !strings.Contains(string(resp.Body), "pending") {
		t.Fatalf("task get: %v", err)
	}
	if u, src := resp.Meter.Usage(); u != nil || src != protocol.UsageUpstream {
		t.Errorf("usage = %v (%v)", u, src)
	}
}

// MU-05：结果地址只允许 https 或与 base_url 同主机。
func TestEgressDo_RejectsForeignResultURL(t *testing.T) {
	_, up, _ := v4Stub(t, func(w http.ResponseWriter, _ string) {
		_, _ = io.WriteString(w, doneState("http://169.254.169.254/latest/meta-data", 1))
	}, nil)
	_, err := fastPoll.Do(context.Background(), up, &protocol.Call{Op: OpTaskResult, Params: map[string]string{"id": "task_1"}})
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeServerError || !strings.Contains(err.Error(), "unsupported result url") {
		t.Errorf("foreign url: %v", err)
	}
}

func TestReadResultZip(t *testing.T) {
	if _, _, err := readResultZip([]byte("x"), 1<<20); err == nil {
		t.Error("non-zip accepted")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("other.txt"); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close() // bytes.Buffer 不会失败
	if _, _, err := readResultZip(buf.Bytes(), 1<<20); err == nil || !strings.Contains(err.Error(), "full.md") {
		t.Errorf("zip without full.md: %v", err)
	}
	big := resultZip(t, "", strings.Repeat("a", 2048), 0)
	if _, _, err := readResultZip(big, 1024); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized full.md: %v", err)
	}
	md, pages, err := readResultZip(resultZip(t, "x/", "# ok", 3), 1<<20)
	if err != nil || string(md) != "# ok" || pages != 3 {
		t.Errorf("readResultZip = %q %d %v", md, pages, err)
	}
}

func TestUntilQuotaReset(t *testing.T) {
	for _, tt := range []struct {
		now  time.Time
		want time.Duration
	}{
		{time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC), 31 * time.Minute},          // 23:30 北京
		{time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC), 24*time.Hour + time.Minute}, // 00:00 北京
		{time.Date(2026, 12, 31, 4, 0, 0, 0, time.UTC), 12*time.Hour + time.Minute},  // 12:00 北京，跨年
		{time.Date(2026, 11, 12, 15, 59, 30, 0, time.UTC), 90 * time.Second},         // 23:59:30 北京
	} {
		if got := untilQuotaReset(tt.now); got != tt.want {
			t.Errorf("untilQuotaReset(%v) = %v, want %v", tt.now, got, tt.want)
		}
	}
}
