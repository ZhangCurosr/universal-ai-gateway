package rest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func i64(v int64) *int64 { return &v }

var cst = time.FixedZone("UTC+08:00", 8*3600)

// mineruDef 是 MinerU 精准解析 API 的声明（与 docs/handoff/T66 的配置示例相同）。
func mineruDef() APIDef {
	return APIDef{
		Service: "mu",
		Errors: []ErrorRule{
			{JSON: []string{"msgCode"}, In: []any{"A0202", "A0211"}, Outcome: protocol.OutcomeAuthFailed},
			{JSON: []string{"code"}, In: []any{-60018, -60019}, Outcome: protocol.OutcomeRateLimited, Cooldown: &Cooldown{Hour: 0, Minute: 1, Loc: cst}},
			{JSON: []string{"code"}, In: []any{-60012, -60013}, Outcome: protocol.OutcomeClientError, Client: 404},
			{JSON: []string{"code"}, NotIn: []any{0}, Outcome: protocol.OutcomeClientError},
		},
		Endpoints: []EndpointDef{
			{Method: "POST", Segments: []string{"api", "v4", "extract", "task"}, TaskID: []string{"data", "task_id"},
				Usage: map[string]UsageRule{"pages": {RequestQuery: "pages", Default: i64(600)}}},
			{Method: "GET", Segments: []string{"api", "v4", "extract", "task", "{task}"}, PinnedBy: "task", Usage: map[string]UsageRule{}},
		},
	}
}

func tavilyDef() APIDef {
	return APIDef{
		Service: "tv",
		Errors:  []ErrorRule{{Status: []int{432, 433}, Outcome: protocol.OutcomeRateLimited, Cooldown: &Cooldown{DayOfMonth: 1, Loc: time.UTC}}},
		Endpoints: []EndpointDef{{Method: "POST", Segments: []string{"search"}, Usage: map[string]UsageRule{
			"credits": {JSON: []string{"usage", "credits"}, RequestJSON: []string{"search_depth"}, Map: map[string]int64{"advanced": 2}, Default: i64(1)},
		}}},
	}
}

func mustTable(t *testing.T, defs ...APIDef) *apiTable {
	t.Helper()
	tb, err := newAPITable(defs)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestLookupPathAndEqual(t *testing.T) {
	doc, ok := decodeJSON([]byte(`{"code":-60018,"data":{"task_id":"t-1","items":[{"id":7},{"id":"x"}]},"s":"1","f":1.5,"b":true,"n":null}`))
	if !ok {
		t.Fatal("decode")
	}
	for _, tc := range []struct {
		path string
		want any
		ok   bool
	}{
		{"code", -60018, true}, {"data.task_id", "t-1", true}, {"data.items.0.id", 7, true}, {"data.items.1.id", "x", true},
		{"data.items.2.id", nil, false}, {"data.items.x", nil, false}, {"code.x", nil, false}, {"missing", nil, false}, {"f", 1.5, true}, {"b", true, true},
	} {
		v, ok := lookupPath(doc, strings.Split(tc.path, "."))
		if ok != tc.ok || (ok && !equalJSON(v, tc.want)) {
			t.Errorf("lookupPath(%s) = %v, %v", tc.path, v, ok)
		}
	}
	s, _ := lookupPath(doc, []string{"s"})
	if equalJSON(s, 1) || !equalJSON(s, "1") { // DA-03：1 与 "1" 不相等
		t.Error(`"1" must not equal 1`)
	}
	c, _ := lookupPath(doc, []string{"code"})
	if equalJSON(c, "-60018") || !equalJSON(c, int64(-60018)) || !equalJSON(c, -60018.0) {
		t.Error("number comparison")
	}
	if n, _ := lookupPath(doc, []string{"n"}); equalJSON(n, "null") || equalJSON(n, 0) {
		t.Error("null must not equal anything")
	}
}

func TestMatchError(t *testing.T) {
	d := mineruDef()
	cases := []struct {
		body   string
		status int
		want   protocol.OutcomeKind
		hit    bool
	}{
		{`{"code":0,"data":{}}`, 200, 0, false},
		{`{"msgCode":"A0202"}`, 200, protocol.OutcomeAuthFailed, true},
		{`{"code":-60019}`, 200, protocol.OutcomeRateLimited, true},
		{`{"code":-60012}`, 200, protocol.OutcomeClientError, true},
		{`{"code":-1}`, 200, protocol.OutcomeClientError, true},
		{`{"msg":"no code"}`, 200, 0, false}, // not_in 在字段不存在时不命中
		{`not json`, 200, 0, false},
	}
	for _, tc := range cases {
		doc, ok := decodeJSON([]byte(tc.body))
		r := d.matchError(tc.status, doc, ok)
		if (r != nil) != tc.hit || (r != nil && r.Outcome != tc.want) {
			t.Errorf("%s: rule = %+v", tc.body, r)
		}
	}
	exists, absent := true, false
	d2 := APIDef{Errors: []ErrorRule{
		{Status: []int{200}, JSON: []string{"error"}, Exists: &exists, Outcome: protocol.OutcomeServerError},
		{Status: []int{500}, Outcome: protocol.OutcomeOverloaded},
		{JSON: []string{"ok"}, Exists: &absent, Outcome: protocol.OutcomeClientError},
	}}
	doc, _ := decodeJSON([]byte(`{"error":"x","ok":1}`))
	if r := d2.matchError(200, doc, true); r == nil || r.Outcome != protocol.OutcomeServerError {
		t.Errorf("exists rule: %+v", r)
	}
	if r := d2.matchError(201, doc, true); r != nil {
		t.Errorf("status mismatch must skip: %+v", r)
	}
	if r := d2.matchError(500, nil, false); r == nil || r.Outcome != protocol.OutcomeOverloaded {
		t.Errorf("status-only rule: %+v", r)
	}
	doc, _ = decodeJSON([]byte(`{}`))
	if r := d2.matchError(201, doc, true); r == nil || r.Outcome != protocol.OutcomeClientError {
		t.Errorf("exists:false rule: %+v", r)
	}
}

func TestCooldownNext(t *testing.T) {
	daily := &Cooldown{Hour: 0, Minute: 1, Loc: cst}
	for in, want := range map[string]string{
		"2026-11-13T10:00:00+08:00": "2026-11-14T00:01:00+08:00",
		"2026-11-13T00:00:30+08:00": "2026-11-13T00:01:00+08:00",
		"2026-11-13T00:01:00+08:00": "2026-11-14T00:01:00+08:00", // 恰好在重置时刻：下一天
		"2026-11-13T17:00:00Z":      "2026-11-15T00:01:00+08:00", // UTC 17:00 = 北京时间次日 01:00
		"2026-12-31T23:59:00+08:00": "2027-01-01T00:01:00+08:00",
	} {
		now, _ := time.Parse(time.RFC3339, in)
		w, _ := time.Parse(time.RFC3339, want)
		if got := daily.next(now); !got.Equal(w) {
			t.Errorf("daily next(%s) = %s, want %s", in, got, want)
		}
	}
	monthly := &Cooldown{DayOfMonth: 1, Loc: time.UTC}
	for in, want := range map[string]string{
		"2026-11-13T10:00:00Z": "2026-12-01T00:00:00Z",
		"2026-12-15T00:00:00Z": "2027-01-01T00:00:00Z",
		"2026-12-01T00:00:00Z": "2027-01-01T00:00:00Z",
		"2026-11-30T23:59:59Z": "2026-12-01T00:00:00Z",
	} {
		now, _ := time.Parse(time.RFC3339, in)
		w, _ := time.Parse(time.RFC3339, want)
		if got := monthly.next(now); !got.Equal(w) {
			t.Errorf("monthly next(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestComputeUsage(t *testing.T) {
	tv := tavilyDef().Endpoints[0]
	resp := func(s string) any { d, _ := decodeJSON([]byte(s)); return d }
	for _, tc := range []struct {
		name      string
		doc, req  any
		want      int64
		src       protocol.UsageSource
		warnCount int
	}{
		{"upstream reports", resp(`{"usage":{"credits":3}}`), resp(`{"search_depth":"advanced"}`), 3, protocol.UsageUpstream, 0},
		{"request map", resp(`{}`), resp(`{"search_depth":"advanced"}`), 2, protocol.UsageEstimated, 0},
		{"not in map → default", resp(`{}`), resp(`{"search_depth":"basic"}`), 1, protocol.UsageEstimated, 0},
		{"no request body → default", resp(`{}`), nil, 1, protocol.UsageEstimated, 0},
		{"bad upstream value", resp(`{"usage":{"credits":-1}}`), nil, 1, protocol.UsageEstimated, 1},
		{"float upstream value", resp(`{"usage":{"credits":1.5}}`), nil, 1, protocol.UsageEstimated, 1},
	} {
		u := computeUsage(&tv, tc.doc, tc.req, nil)
		if u.usage["credits"] != tc.want || u.src != tc.src || len(u.warn) != tc.warnCount {
			t.Errorf("%s: %+v", tc.name, u)
		}
	}
	if u := computeUsage(nil, nil, nil, nil); u.usage[UnitRequests] != 1 || u.src != protocol.UsageUpstream {
		t.Errorf("no endpoint: %+v", u)
	}
	if u := computeUsage(&EndpointDef{Usage: map[string]UsageRule{}}, nil, nil, nil); u.usage != nil || u.src != protocol.UsageUpstream {
		t.Errorf("usage {}: %+v", u)
	}
	none := EndpointDef{Usage: map[string]UsageRule{"pages": {JSON: []string{"pages"}}}}
	if u := computeUsage(&none, resp(`{}`), nil, nil); u.usage != nil || u.src != protocol.UsageNone {
		t.Errorf("nothing found: %+v", u)
	}
	mu := mineruDef().Endpoints[0]
	if u := computeUsage(&mu, nil, nil, url.Values{"pages": {"13"}}); u.usage["pages"] != 13 || u.src != protocol.UsageEstimated {
		t.Errorf("request_query: %+v", u)
	}
	if u := computeUsage(&mu, nil, nil, url.Values{"pages": {"abc"}}); u.usage["pages"] != 600 || len(u.warn) != 1 {
		t.Errorf("bad request_query: %+v", u)
	}
	if u := computeUsage(&mu, nil, nil, nil); u.usage["pages"] != 600 {
		t.Errorf("default: %+v", u)
	}
}

func TestEndpointMatch(t *testing.T) {
	d := mineruDef()
	if ep, vars := d.match("GET", "/api/v4/extract/task/t%2D9"); ep == nil || vars["task"] != "t-9" {
		t.Fatalf("match = %v %v", ep, vars)
	}
	if ep, _ := d.match("HEAD", "/api/v4/extract/task/x"); ep == nil {
		t.Fatal("HEAD must match GET")
	}
	for _, tc := range [][2]string{{"GET", "/api/v4/extract/task"}, {"GET", "/api/v4/extract/task/x/y"}, {"DELETE", "/api/v4/extract/task/x"}, {"GET", "/api/v4/extract/task/"}} {
		if ep, _ := d.match(tc[0], tc[1]); ep != nil {
			t.Errorf("%v matched", tc)
		}
	}
	var nilDef *APIDef
	if ep, _ := nilDef.match("GET", "/x"); ep != nil {
		t.Error("nil def matched")
	}
}

func TestAPITableUnits(t *testing.T) {
	tb := mustTable(t, mineruDef(), tavilyDef())
	if got := strings.Join(tb.Units(), ","); got != "credits,pages,requests" {
		t.Fatalf("Units() = %s", got)
	}
	var none *apiTable
	if got := strings.Join(none.Units(), ","); got != "requests" {
		t.Fatalf("nil Units() = %s", got)
	}
	if _, err := newAPITable([]APIDef{mineruDef(), mineruDef()}); err == nil {
		t.Fatal("duplicate service accepted")
	}
	if _, err := newAPITable([]APIDef{{}}); err == nil {
		t.Fatal("empty service accepted")
	}
	if _, err := newAPITable([]APIDef{{Service: "x", Errors: []ErrorRule{{Cooldown: &Cooldown{}}}}}); err == nil {
		t.Fatal("cooldown without zone accepted")
	}
}

// --- 出站：经 httptest 上游的完整一次尝试 ---

type apiHarness struct {
	eg   Egress
	in   Ingress
	srv  *httptest.Server
	now  time.Time
	resp *protocol.Response
}

func newAPIHarness(t *testing.T, h http.HandlerFunc, defs ...APIDef) *apiHarness {
	t.Helper()
	tb := mustTable(t, defs...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	x := &apiHarness{srv: srv, in: Ingress{apis: tb}, now: time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)}
	x.eg = NewEgress()
	x.eg.apis = tb
	x.eg.now = func() time.Time { return x.now }
	return x
}

// do 模拟一次请求：Parse → Prepare → Do。
func (x *apiHarness) do(t *testing.T, method, target string, body string) (*protocol.Envelope, *protocol.Response, error) {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	op, kind := OpGet, protocol.BodyNone
	switch {
	case strings.HasPrefix(target, "/rest-idem/") && method == http.MethodPost:
		op, kind = OpPostIdem, protocol.BodyBuffered
	case method == http.MethodPost:
		op, kind = OpPost, protocol.BodyStream
	}
	mux := http.NewServeMux()
	var env *protocol.Envelope
	var perr error
	pattern := "/rest/{service}/{path...}"
	if strings.HasPrefix(target, "/rest-idem/") {
		pattern = "/rest-idem/{service}/{path...}"
	}
	mux.HandleFunc(pattern, func(_ http.ResponseWriter, r *http.Request) {
		var b []byte
		if kind == protocol.BodyBuffered {
			b, _ = io.ReadAll(r.Body)
		}
		env, perr = x.in.Parse(r, protocol.Route{Op: op, Body: kind}, b)
		if env != nil && kind == protocol.BodyStream {
			env.BodyStream = r.Body
		}
		if perr != nil {
			return
		}
		call, err := x.eg.Prepare(env, env.Model)
		if err != nil {
			perr = err
			return
		}
		resp, err := x.eg.Do(context.Background(), protocol.Upstream{BaseURL: x.srv.URL, Secret: []byte("sk-test-123456"), Client: x.srv.Client(), MaxBufferedBytes: 1 << 10}, call)
		if resp != nil && resp.Kind == protocol.ResponseRaw && resp.Raw != nil {
			raw, _ := io.ReadAll(resp.Raw)
			_ = resp.Raw.Close()
			resp.Body = raw
		}
		x.resp, perr = resp, err
	})
	mux.ServeHTTP(httptest.NewRecorder(), r)
	return env, x.resp, perr
}

func usageOf(r *protocol.Response) (map[string]int64, protocol.UsageSource) {
	u, src := r.Meter.Usage()
	return u, src
}

func TestDeclaredMinerUFlow(t *testing.T) {
	var polls int
	x := newAPIHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v4/extract/task":
			_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"abc-1"},"msg":"ok"}`)
		case r.Method == "GET" && r.URL.Path == "/api/v4/extract/task/abc-1":
			polls++
			_, _ = io.WriteString(w, `{"code":0,"data":{"state":"done"}}`)
		case r.URL.Path == "/quota":
			_, _ = io.WriteString(w, `{"code":-60018,"msg":"daily limit"}`)
		case r.URL.Path == "/badtoken":
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"msgCode":"A0202","msg":"token error"}`)
		case r.URL.Path == "/gone":
			_, _ = io.WriteString(w, `{"code":-60012,"msg":"task not found"}`)
		default:
			_, _ = io.WriteString(w, `{"code":0}`)
		}
	}, mineruDef())

	env, resp, err := x.do(t, "POST", "/rest-idem/mu/api/v4/extract/task?pages=13", `{"url":"https://example.com/a.pdf"}`)
	if err != nil || resp.Kind != protocol.ResponseBuffered || len(resp.Pins) != 1 || resp.Pins[0] != "mu:abc-1" {
		t.Fatalf("submit: %v %+v", err, resp)
	}
	if u, src := usageOf(resp); u["pages"] != 13 || src != protocol.UsageEstimated || len(env.Estimate) != 0 {
		t.Fatalf("submit usage = %v %v, estimate %v", u, src, env.Estimate)
	}

	env, resp, err = x.do(t, "GET", "/rest/mu/api/v4/extract/task/abc-1", "")
	if err != nil || !env.Affinity.Strict || string(env.Affinity.Value) != "mu:abc-1" || env.Affinity.Source != protocol.AffinityPin {
		t.Fatalf("poll: %v %+v", err, env.Affinity)
	}
	if u, src := usageOf(resp); u != nil || src != protocol.UsageUpstream || polls != 1 {
		t.Fatalf("poll usage = %v %v (polls %d)", u, src, polls)
	}

	_, _, err = x.do(t, "GET", "/rest/mu/quota", "")
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeRateLimited || ae.Outcome.Status != 429 {
		t.Fatalf("quota: %v", err)
	}
	if want := 14*time.Hour + time.Minute; ae.Outcome.RetryAfter != want { // 北京时间 10:00 → 次日 00:01
		t.Fatalf("RetryAfter = %v, want %v", ae.Outcome.RetryAfter, want)
	}

	_, _, err = x.do(t, "GET", "/rest/mu/badtoken", "")
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeAuthFailed {
		t.Fatalf("bad token: %v", err)
	}

	_, _, err = x.do(t, "GET", "/rest/mu/gone", "")
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeClientError || ae.Outcome.Status != 404 || !strings.Contains(string(ae.Body), "-60012") || ae.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("gone: %v %+v", err, ae)
	}

	_, resp, err = x.do(t, "GET", "/rest/mu/other", "")
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := usageOf(resp); u[UnitRequests] != 1 {
		t.Fatalf("unmatched endpoint usage = %v", u)
	}
}

func TestDeclaredTavilyAndStatusRules(t *testing.T) {
	x := newAPIHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("over") == "1" {
			w.WriteHeader(432)
			_, _ = io.WriteString(w, `{"detail":"plan limit"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`) // 没有 Content-Type：按可能是 JSON 处理
	}, tavilyDef())

	_, resp, err := x.do(t, "POST", "/rest-idem/tv/search", `{"query":"q","search_depth":"advanced"}`)
	if u, src := usageOf(resp); err != nil || u["credits"] != 2 || src != protocol.UsageEstimated {
		t.Fatalf("advanced: %v %v", err, u)
	}
	// 流式请求体读不到请求字段：取缺省值（DA-12）
	_, resp, err = x.do(t, "POST", "/rest/tv/search", `{"query":"q","search_depth":"advanced"}`)
	if u, _ := usageOf(resp); err != nil || u["credits"] != 1 || resp.Kind != protocol.ResponseRaw {
		t.Fatalf("streamed: %v %v %v", err, u, resp.Kind)
	}
	_, _, err = x.do(t, "POST", "/rest-idem/tv/search?over=1", `{}`)
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeRateLimited || ae.Outcome.Status != 432 {
		t.Fatalf("432: %v", err)
	}
	if want := 18*24*time.Hour - 2*time.Hour; ae.Outcome.RetryAfter != want { // 11-13 02:00 UTC → 12-01 00:00 UTC
		t.Fatalf("RetryAfter = %v, want %v", ae.Outcome.RetryAfter, want)
	}
}

func TestDeclaredPassThroughAndLimits(t *testing.T) {
	big := strings.Repeat("x", 4<<10)
	x := newAPIHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = io.WriteString(w, big)
		case "/big":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"pad":"`+big+`"}`)
		case "/denied":
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"code":"P403"}`)
		case "/denied-plain":
			w.WriteHeader(403)
		}
	}, APIDef{Service: "s", Errors: []ErrorRule{
		{JSON: []string{"code"}, In: []any{"P403"}, Outcome: protocol.OutcomePermissionDenied},
		{JSON: []string{"code"}, NotIn: []any{0}, Outcome: protocol.OutcomeClientError},
	}})
	// 非 JSON 响应不缓冲，原样流式转发（DA-11）
	_, resp, err := x.do(t, "GET", "/rest/s/file", "")
	if err != nil || resp.Kind != protocol.ResponseRaw || len(resp.Body) != len(big) {
		t.Fatalf("file: %v %+v", err, resp)
	}
	// JSON 超过缓冲上限：local_error，不惩罚 Key
	_, _, err = x.do(t, "GET", "/rest/s/big", "")
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeLocalError || !errors.Is(err, protocol.ErrResponseTooLarge) {
		t.Fatalf("big: %v", err)
	}
	// 规则映射为 permission_denied：按 RS-16 不隔离 Key
	_, _, err = x.do(t, "GET", "/rest/s/denied", "")
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeClientError || ae.Outcome.Status != 403 {
		t.Fatalf("denied: %v", err)
	}
	_, _, err = x.do(t, "GET", "/rest/s/denied-plain", "")
	if !errors.As(err, &ae) || ae.Outcome.Kind != protocol.OutcomeClientError || ae.Outcome.Status != 403 {
		t.Fatalf("denied plain: %v", err)
	}
	// 没有声明的服务：行为不变
	_, resp, err = x.do(t, "GET", "/rest/other/file", "")
	if u, _ := usageOf(resp); err != nil || resp.Kind != protocol.ResponseRaw || u[UnitRequests] != 1 {
		t.Fatalf("undeclared: %v %v", err, u)
	}
}

func TestDeclaredTaskIDMissing(t *testing.T) {
	x := newAPIHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":{"nested":1}}}`)
	}, mineruDef())
	_, resp, err := x.do(t, "POST", "/rest-idem/mu/api/v4/extract/task", `{}`)
	if err != nil || len(resp.Pins) != 0 {
		t.Fatalf("non-scalar task id must not pin: %v %+v", err, resp)
	}
}
