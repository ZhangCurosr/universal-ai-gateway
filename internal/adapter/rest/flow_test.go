package rest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// muFlowDef 用 flow 表达 MinerU 的三种用法（与 docs/handoff/T67 的配置示例相同）。
func muFlowDef(storage string) APIDef {
	d := mineruDef()
	d.Endpoints = append([]EndpointDef{
		{Method: "POST", Segments: []string{"parse"}, Usage: map[string]UsageRule{"pages": {JSON: []string{"data", "extract_progress", "total_pages"},
			ZipJSON: &ZipCount{Entry: "layout.json", Count: []string{"pdf_info"}}}},
			Flow: &FlowDef{
				Submit: &FlowStep{Method: "POST", Path: "/api/v4/extract/task", TaskID: []string{"data", "task_id"}},
				Poll:   &FlowPoll{Method: "GET", Path: "/api/v4/extract/task/{task_id}", State: []string{"data", "state"}, Done: []string{"done"}, Failed: []string{"failed"}, Timeout: time.Minute},
				Result: &FlowResult{URL: []string{"data", "full_zip_url"}, ZipEntry: "full.md", ContentType: "text/markdown; charset=utf-8"},
			}},
		{Method: "POST", Segments: []string{"parse", "file"}, Usage: map[string]UsageRule{"pages": {JSON: []string{"data", "extract_result", "0", "extract_progress", "total_pages"}}},
			Flow: &FlowDef{
				Upload: &FlowUpload{FlowStep: FlowStep{Method: "POST", Path: "/api/v4/file-urls/batch", TaskID: []string{"data", "batch_id"}},
					UploadURL: []string{"data", "file_urls", "0"},
					Body: []BodyField{
						{Target: []string{"files", "0", "name"}, Query: "name", Type: "string", Required: true},
						{Target: []string{"files", "0", "is_ocr"}, Query: "is_ocr", Type: "bool"},
						{Target: []string{"extra_formats"}, Query: "extra_formats", Type: "list"},
					}},
				Poll:   &FlowPoll{Method: "GET", Path: "/api/v4/extract-results/batch/{task_id}", State: []string{"data", "extract_result", "0", "state"}, Done: []string{"done"}, Failed: []string{"failed"}},
				Result: &FlowResult{URL: []string{"data", "extract_result", "0", "full_zip_url"}, ZipEntry: "full.md", ContentType: "text/markdown; charset=utf-8"},
			}},
		{Method: "POST", Segments: []string{"tasks", "file"}, Usage: map[string]UsageRule{"pages": {RequestQuery: "pages", Default: i64(600)}},
			Flow: &FlowDef{Upload: &FlowUpload{FlowStep: FlowStep{Method: "POST", Path: "/api/v4/file-urls/batch", TaskID: []string{"data", "batch_id"}},
				UploadURL: []string{"data", "file_urls", "0"}, Body: []BodyField{{Target: []string{"files", "0", "name"}, Query: "name", Type: "string", Required: true}}}}},
	}, d.Endpoints...)
	return d
}

type flowHarness struct {
	eg      Egress
	in      Ingress
	api     *httptest.Server // 上游（http）
	store   *httptest.Server // 第三方存储（https）
	client  *http.Client
	now     time.Time
	limit   int64
	mu      sync.Mutex
	polls   int
	state   []string // 依次返回的状态；用完后一直返回最后一个
	pages   string   // 轮询响应中的 total_pages（JSON 原文）
	zipBody []byte
	puts    []*http.Request
	putBody []byte
	applied map[string]any
	zipURL  string // 结果地址；空 = 上游同源
	submit  string // 提交响应
}

func newFlowHarness(t *testing.T) *flowHarness {
	t.Helper()
	x := &flowHarness{now: time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC), limit: 1 << 20, state: []string{"pending", "running", "done"}, pages: "4",
		zipBody: zipOf(t, map[string]string{"sub/full.md": "# Hello\n\nsecond page", "sub/layout.json": "{}"}), submit: `{"code":0,"data":{"task_id":"t1"}}`}
	x.store = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			x.puts = append(x.puts, r.Clone(context.Background()))
			x.putBody = b
			x.polls = 0
		case http.MethodGet:
			if r.Header.Get("Authorization") != "" {
				w.WriteHeader(http.StatusForbidden) // 第三方存储不得收到上游 Key
				return
			}
			_, _ = w.Write(x.zipBody)
		}
	}))
	t.Cleanup(x.store.Close)
	x.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk-test-123456" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		zipURL := x.zipURL
		if zipURL == "" {
			zipURL = "http://" + r.Host + "/zip/t1"
		}
		next := func() string {
			s := x.state[min(x.polls, len(x.state)-1)]
			x.polls++
			return s
		}
		switch {
		case r.URL.Path == "/api/v4/extract/task" && r.Method == "POST":
			_, _ = io.WriteString(w, x.submit)
		case strings.HasPrefix(r.URL.Path, "/api/v4/extract/task/"):
			s := next()
			_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"t1","state":"`+s+`","full_zip_url":"`+zipURL+`","extract_progress":{"total_pages":`+x.pages+`},"err_msg":"bad file"}}`)
		case r.URL.Path == "/api/v4/file-urls/batch":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body) // 解析失败时 applied 为空，由断言报告
			x.applied = body
			_, _ = io.WriteString(w, `{"code":0,"data":{"batch_id":"b1","file_urls":["`+x.store.URL+`/upload/b1?Signature=s3cr3t"]}}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/extract-results/batch/"):
			if x.polls == 0 {
				x.polls++
				_, _ = io.WriteString(w, `{"code":0,"data":{"batch_id":"b1","extract_result":[]}}`)
				return
			}
			s := next()
			_, _ = io.WriteString(w, `{"code":0,"data":{"batch_id":"b1","extract_result":[{"state":"`+s+`","full_zip_url":"`+x.store.URL+`/zip/b1","extract_progress":{"total_pages":2}}]}}`)
		case r.URL.Path == "/zip/t1":
			_, _ = w.Write(x.zipBody)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(x.api.Close)
	x.client = x.store.Client() // 信任测试证书，同时能访问 http 上游
	tb := mustTable(t, muFlowDef(x.store.URL))
	x.in = Ingress{apis: tb}
	x.eg = NewEgress()
	x.eg.apis = tb
	x.eg.now = func() time.Time { return x.now }
	x.eg.sleep = func(ctx context.Context, d time.Duration) error { x.now = x.now.Add(d); return ctx.Err() }
	return x
}

// run 模拟一次请求：Parse → Prepare → Do；ResponseRaw 读成 Body。contentLength < 0 表示分块。
func (x *flowHarness) run(t *testing.T, target, body string, contentLength int64) (*protocol.Envelope, *protocol.Response, error) {
	t.Helper()
	idem := strings.HasPrefix(target, "/rest-idem/")
	op, kind, pattern := OpPost, protocol.BodyStream, "/rest/{service}/{path...}"
	if idem {
		op, kind, pattern = OpPostIdem, protocol.BodyBuffered, "/rest-idem/{service}/{path...}"
	}
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r.ContentLength = contentLength
	var env *protocol.Envelope
	var resp *protocol.Response
	var rerr error
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, func(_ http.ResponseWriter, r *http.Request) {
		var b []byte
		if kind == protocol.BodyBuffered {
			b, _ = io.ReadAll(r.Body)
		}
		env, rerr = x.in.Parse(r, protocol.Route{Op: op, Body: kind}, b)
		if rerr != nil {
			return
		}
		if kind == protocol.BodyStream {
			env.BodyStream = r.Body
		}
		call, err := x.eg.Prepare(env, env.Model)
		if err != nil {
			rerr = err
			return
		}
		resp, rerr = x.eg.Do(context.Background(), protocol.Upstream{BaseURL: x.api.URL, Secret: []byte("sk-test-123456"), Client: x.client, MaxBufferedBytes: x.limit}, call)
		if resp != nil && resp.Kind == protocol.ResponseRaw && resp.Raw != nil {
			resp.Body, _ = io.ReadAll(resp.Raw)
			_ = resp.Raw.Close()
		}
	})
	mux.ServeHTTP(httptest.NewRecorder(), r)
	return env, resp, rerr
}

func TestFlowSyncSubmit(t *testing.T) {
	x := newFlowHarness(t)
	_, resp, err := x.run(t, "/rest-idem/mu/parse", `{"url":"https://example.com/a.pdf"}`, -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Kind != protocol.ResponseBuffered || resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" || string(resp.Body) != "# Hello\n\nsecond page" {
		t.Fatalf("result = %+v %q", resp.Header, resp.Body)
	}
	if u, src := resp.Meter.Usage(); u["pages"] != 4 || src != protocol.UsageUpstream || len(resp.Pins) != 1 || resp.Pins[0] != "mu:t1" {
		t.Fatalf("usage %v %v pins %v", u, src, resp.Pins)
	}
	if x.polls != 3 {
		t.Fatalf("polls = %d", x.polls)
	}
	if !x.now.After(time.Date(2026, 11, 13, 2, 0, 6, 0, time.UTC)) { // 2s + 3s + 4.5s
		t.Fatalf("poll waits did not advance: %v", x.now)
	}
}

func TestFlowFailuresAfterSubmitKeepMetering(t *testing.T) {
	cases := []struct {
		name  string
		setup func(x *flowHarness)
		kind  protocol.OutcomeKind
		want  string
	}{
		{"task failed", func(x *flowHarness) { x.state = []string{"running", "failed"} }, protocol.OutcomeClientError, "bad file"},
		{"timeout", func(x *flowHarness) { x.state = []string{"running"} }, protocol.OutcomeLocalError, ""},
		{"zip entry missing", func(x *flowHarness) { x.zipBody = zipOf(t, map[string]string{"other.md": "x"}) }, protocol.OutcomeLocalError, ""}, // FL-08：任务已创建，改写为不可重试、不惩罚 Key
		{"not a zip", func(x *flowHarness) { x.zipBody = []byte("plain") }, protocol.OutcomeLocalError, ""},                                      // FL-08：任务已创建，改写为不可重试、不惩罚 Key
		{"zip too large", func(x *flowHarness) { x.limit = 64 }, protocol.OutcomeLocalError, ""},
		{"insecure third-party url", func(x *flowHarness) { x.zipURL = "http://cdn.example.com/a.zip" }, protocol.OutcomeLocalError, ""}, // FL-08：任务已创建，改写为不可重试、不惩罚 Key
		{"no task id", func(x *flowHarness) { x.submit = `{"code":0,"data":{}}` }, protocol.OutcomeLocalError, ""},                       // FL-08：任务已创建，改写为不可重试、不惩罚 Key
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newFlowHarness(t)
			tc.setup(x)
			_, resp, err := x.run(t, "/rest-idem/mu/parse", `{"url":"https://example.com/a.pdf"}`, -1)
			var ae *protocol.AttemptError
			if !errors.As(err, &ae) || ae.Outcome.Kind != tc.kind || ae.Outcome.Kind.Retryable() {
				t.Fatalf("err = %v", err)
			}
			if tc.want != "" && !strings.Contains(string(ae.Body), tc.want) {
				t.Fatalf("error body = %q", ae.Body)
			}
			if resp == nil || resp.Meter == nil || resp.Body != nil {
				t.Fatalf("paid failure must carry only a meter (CORE-12): %+v", resp)
			}
		})
	}
}

func TestFlowSubmitRejectedIsRetryable(t *testing.T) {
	x := newFlowHarness(t)
	x.submit = `{"code":-60018,"msg":"daily quota"}`
	_, resp, err := x.run(t, "/rest-idem/mu/parse", `{"url":"u"}`, -1)
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeRateLimited || resp != nil || x.polls != 0 {
		t.Fatalf("err = %v resp = %v polls %d", err, resp, x.polls)
	}
	// 流式请求体上的提交 flow：400，提示改用 /rest-idem/
	if _, _, err := x.run(t, "/rest/mu/parse", `{"url":"u"}`, 11); !strings.Contains(err.Error(), "/rest-idem/") {
		t.Fatalf("streamed submit: %v", err)
	}
}

func TestFlowUploadSync(t *testing.T) {
	x := newFlowHarness(t)
	file := "%PDF-1.4 two pages"
	_, resp, err := x.run(t, "/rest/mu/parse/file?name=%E6%B5%8B%E8%AF%95.pdf&is_ocr=true&extra_formats=docx,%20html", file, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "# Hello\n\nsecond page" {
		t.Fatalf("result = %q", resp.Body)
	}
	if u, _ := resp.Meter.Usage(); u["pages"] != 2 || resp.Pins[0] != "mu:b1" {
		t.Fatalf("usage %v pins %v", u, resp.Pins)
	}
	files, _ := x.applied["files"].([]any)
	f0, _ := files[0].(map[string]any)
	if f0["name"] != "测试.pdf" || f0["is_ocr"] != true || len(x.applied["extra_formats"].([]any)) != 2 {
		t.Fatalf("apply body = %v", x.applied)
	}
	put := x.puts[0]
	if put.Header.Get("Authorization") != "" || put.Header.Get("Content-Type") != "" || put.ContentLength != int64(len(file)) || string(x.putBody) != file {
		t.Fatalf("PUT = %v len %d body %q", put.Header, put.ContentLength, x.putBody)
	}
}

func TestFlowUploadAsyncAndInputs(t *testing.T) {
	x := newFlowHarness(t)
	_, resp, err := x.run(t, "/rest/mu/tasks/file?name=a.pdf&pages=3", "file", 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"task_id":"b1"}` || strings.Contains(string(resp.Body), "Signature") || resp.Pins[0] != "mu:b1" {
		t.Fatalf("async upload = %s %v", resp.Body, resp.Pins)
	}
	if u, src := resp.Meter.Usage(); u["pages"] != 3 || src != protocol.UsageEstimated {
		t.Fatalf("usage = %v %v", u, src)
	}
	if _, _, err := x.run(t, "/rest/mu/tasks/file?name=a.pdf", "file", -1); err == nil || !strings.Contains(err.Error(), "Content-Length") {
		t.Fatalf("chunked: %v", err)
	}
	if _, _, err := x.run(t, "/rest/mu/tasks/file", "file", 4); err == nil || !strings.Contains(err.Error(), `"name" is required`) {
		t.Fatalf("missing name: %v", err)
	}
	if _, _, err := x.run(t, "/rest/mu/parse/file?name=a.pdf&is_ocr=maybe", "file", 4); err == nil || !strings.Contains(err.Error(), `"is_ocr" must be true or false`) {
		t.Fatalf("bad bool: %v", err)
	}
	if len(x.puts) != 1 {
		t.Fatalf("invalid requests must not upload: %d PUTs", len(x.puts))
	}
}

func TestBuildBody(t *testing.T) {
	fields := []BodyField{
		{Target: []string{"files", "0", "name"}, Query: "name", Type: "string", Required: true},
		{Target: []string{"files", "0", "pages"}, Query: "n", Type: "int"},
		{Target: []string{"tags"}, Query: "tags", Type: "list"},
	}
	got, err := buildBody(fields, url.Values{"name": {"a.pdf"}, "n": {"7"}, "tags": {"a,,b "}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"files":[{"name":"a.pdf","pages":7}],"tags":["a","b"]}` {
		t.Fatalf("body = %s", b)
	}
	if _, err := buildBody(fields, url.Values{"name": {"a"}, "n": {"x"}}); err == nil || !strings.Contains(err.Message, "integer") {
		t.Fatalf("bad int: %v", err)
	}
	if _, err := buildBody([]BodyField{{Target: []string{"a", "1"}, Query: "q", Type: "string"}}, url.Values{"q": {"x"}}); err == nil {
		t.Fatal("index gap accepted")
	}
	if _, err := buildBody([]BodyField{{Target: []string{"a"}, Query: "q"}, {Target: []string{"a", "b"}, Query: "r"}}, url.Values{"q": {"x"}, "r": {"y"}}); err == nil {
		t.Fatal("conflicting paths accepted")
	}
	if err := validateFlowDef("s", &FlowDef{Upload: &FlowUpload{Body: []BodyField{{Target: []string{"files", "1", "n"}, Query: "q"}}}}); err == nil {
		t.Fatal("validateFlowDef accepted an index gap")
	}
	if err := validateFlowDef("s", &FlowDef{}); err == nil {
		t.Fatal("flow without a first step accepted")
	}
}

func TestAllowedURL(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		ok, sameOK bool
	}{
		{"https://cdn.example.com/a.zip", true, false},
		{"http://api.example.com/a.zip", true, true},
		{"http://cdn.example.com/a.zip", false, false},
		{"ftp://cdn.example.com/a", false, false},
		{"/relative", false, false},
	} {
		_, same, ok := allowedURL(tc.raw, "http://api.example.com")
		if ok != tc.ok || (ok && same != tc.sameOK) {
			t.Errorf("allowedURL(%s) = %v %v", tc.raw, same, ok)
		}
	}
}

// FL-09a：轮询中没有页数（小文件直接从 pending 到 done）时，数结果包里 layout.json 的 pdf_info；轮询中见到的值优先。
func TestFlowUsageFromZip(t *testing.T) {
	x := newFlowHarness(t)
	x.pages = "null"
	x.zipBody = zipOf(t, map[string]string{"r/full.md": "# A", "r/layout.json": `{"pdf_info":[{},{},{}]}`})
	_, resp, err := x.run(t, "/rest-idem/mu/parse", `{"url":"u"}`, -1)
	if err != nil {
		t.Fatal(err)
	}
	if u, src := resp.Meter.Usage(); u["pages"] != 3 || src != protocol.UsageUpstream {
		t.Fatalf("pages from zip = %v %v", u, src)
	}

	x = newFlowHarness(t) // 轮询中见到 4 页：不看结果包
	x.zipBody = zipOf(t, map[string]string{"full.md": "# A", "layout.json": `{"pdf_info":[{},{},{}]}`})
	_, resp, err = x.run(t, "/rest-idem/mu/parse", `{"url":"u"}`, -1)
	if u, _ := resp.Meter.Usage(); err != nil || u["pages"] != 4 {
		t.Fatalf("poll value must win: %v %v", u, err)
	}

	x = newFlowHarness(t) // 结果包里没有 layout.json，轮询中也没有：按预留结算
	x.pages = "null"
	_, resp, err = x.run(t, "/rest-idem/mu/parse", `{"url":"u"}`, -1)
	if u, src := resp.Meter.Usage(); err != nil || u != nil || src != protocol.UsageNone {
		t.Fatalf("no source: %v %v %v", u, src, err)
	}

	x = newFlowHarness(t) // pdf_info 是数字时取原值；坏 JSON 不计
	x.pages = "null"
	x.zipBody = zipOf(t, map[string]string{"full.md": "# A", "layout.json": `{"pdf_info":5}`})
	if _, resp, _ = x.run(t, "/rest-idem/mu/parse", `{"url":"u"}`, -1); func() int64 { u, _ := resp.Meter.Usage(); return u["pages"] }() != 5 {
		t.Fatal("numeric count")
	}
}
