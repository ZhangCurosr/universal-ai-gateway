// Package adaptertest 是协议适配器的一致性测试套件（spec/08 §5）。
//
// 每个适配器包都必须在自己的 conformance_test.go 中调用 RunIngress 与 RunEgress，
// 并提供本协议的样例（Fixture）。套件只通过 protocol 包的接口调用适配器，不假设请求是 JSON、
// 不假设按 token 计费、不假设一次尝试只有一次上游调用，因此同一套检查对大模型 API 与
// 非大模型 API（搜索、文档解析、异步任务……）都成立。
//
// 本包只供测试使用；非测试代码禁止导入（由 archtest 检查）。
package adaptertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

const (
	// SuiteModel 是入站样例默认期望解析出的服务名。
	SuiteModel = "suite-model"
	// SuiteUpstreamModel 是套件调用 Egress.Prepare 时传入的上游服务名。
	SuiteUpstreamModel = "suite-upstream-model"
	// UpstreamPrefix 是假上游 base_url 的路径前缀；交给 EgressCase.Upstream 前已去掉。
	UpstreamPrefix = "/prefix"

	clientToken    = "uai_live_SUITE_CLIENT_TOKEN"
	upstreamSecret = "sk-suite-upstream-secret"
)

// ---------------------------------------------------------------- Ingress

// IngressCase 是一个入站请求样例。
type IngressCase struct {
	Name   string
	Op     string      // 命中的路由（按 Route.Op 查找）
	Path   string      // 实际请求路径；空 = 路由的 Path（路由含通配段时必须填写）
	Query  string      // 查询串，不含 "?"
	Header http.Header // 额外请求头；有请求体且未设置 Content-Type 时默认 application/json
	Body   []byte

	WantModel    string            // 空 = SuiteModel
	WantNoModel  bool              // 期望 Envelope.Model 为空（只允许与 WantStrict 一起使用，IN-G03）
	WantStream   bool              //
	WantEstimate pricing.Usage     // nil = 不检查；否则必须完全相同
	WantParams   map[string]string // nil = 不检查；否则每一项都必须出现在 Envelope.Params 中
	WantStrict   bool              // 期望 Envelope.Affinity.Strict
}

// IngressFixture 是入站适配器的样例。
type IngressFixture struct {
	// Valid 必须覆盖每一个 RouteCall 的 Op（CONF-I02）。
	Valid []IngressCase
	// Invalid 是必须被拒绝为 invalid_request 的请求，至少 1 个（如缺少服务名、请求体格式错误）。
	Invalid []IngressCase
}

// RunIngress 对入站适配器执行一致性检查（CONF-I01 ~ CONF-I07）。
func RunIngress(t *testing.T, in protocol.Ingress, fx IngressFixture) {
	t.Helper()
	routes := map[string]protocol.Route{}
	var catalog *protocol.Route

	t.Run("CONF-I01 routes", func(t *testing.T) {
		for _, r := range in.Routes() {
			if !strings.HasPrefix(r.Path, "/") || r.Method == "" {
				t.Errorf("invalid route %+v", r)
			}
			switch r.Kind {
			case protocol.RouteCall:
				if r.Op == "" || routes[r.Op].Path != "" {
					t.Errorf("call route %s %s: op %q empty or duplicated", r.Method, r.Path, r.Op)
				}
				routes[r.Op] = r
			case protocol.RouteCatalog:
				if catalog != nil {
					t.Errorf("more than one catalog route")
				}
				rc := r
				catalog = &rc
			}
		}
		if len(routes) == 0 {
			t.Fatal("no call routes")
		}
	})

	t.Run("CONF-I02 fixture covers every op", func(t *testing.T) {
		covered := map[string]bool{}
		for _, c := range fx.Valid {
			covered[c.Op] = true
		}
		for op := range routes {
			if !covered[op] {
				t.Errorf("no valid IngressCase for op %q", op)
			}
		}
		if len(fx.Invalid) == 0 {
			t.Error("fixture has no Invalid cases")
		}
	})

	t.Run("CONF-I03 parse valid requests", func(t *testing.T) {
		for _, c := range fx.Valid {
			rt, ok := routes[c.Op]
			if !ok {
				t.Errorf("%s: unknown op %q", c.Name, c.Op)
				continue
			}
			env, err := parseVia(t, in, rt, c)
			if err != nil {
				t.Errorf("%s: Parse() error = %v", c.Name, err)
				continue
			}
			checkEnvelope(t, in, rt, c, env)
		}
	})

	t.Run("CONF-I04 reject invalid requests", func(t *testing.T) {
		for _, c := range fx.Invalid {
			rt, ok := routes[c.Op]
			if !ok {
				t.Errorf("%s: unknown op %q", c.Name, c.Op)
				continue
			}
			if _, err := parseVia(t, in, rt, c); apperr.CodeOf(err) != apperr.CodeInvalidRequest {
				t.Errorf("%s: Parse() error = %v, want invalid_request", c.Name, err)
			}
		}
	})

	t.Run("CONF-I05 write error", func(t *testing.T) {
		for _, code := range []apperr.Code{
			apperr.CodeInvalidRequest, apperr.CodeUnauthenticated, apperr.CodeInsufficientQuota,
			apperr.CodePermissionDenied, apperr.CodeNotFound, apperr.CodeRateLimited, apperr.CodeInternal,
			apperr.CodeUpstreamUnavailable, apperr.CodeOverloaded, apperr.CodeTimeout,
		} {
			rec := httptest.NewRecorder()
			in.WriteError(rec, "req_suite", apperr.New(code, "suite message "+string(code)).WithRetryAfter(3*time.Second))
			if rec.Code != code.HTTPStatus() {
				t.Errorf("%s: status = %d, want %d", code, rec.Code, code.HTTPStatus())
			}
			if rec.Header().Get("Retry-After") != "3" {
				t.Errorf("%s: Retry-After = %q, want 3", code, rec.Header().Get("Retry-After"))
			}
			if rec.Header().Get("Content-Type") == "" || !strings.Contains(rec.Body.String(), "suite message "+string(code)) {
				t.Errorf("%s: content-type=%q body=%s", code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
		}
	})

	t.Run("CONF-I06 stream error event", func(t *testing.T) {
		ev := in.StreamError(apperr.New(apperr.CodeUpstreamError, "upstream stream interrupted"))
		if !bytes.Contains(ev.Data, []byte("upstream stream interrupted")) {
			t.Errorf("StreamError() data = %s", ev.Data)
		}
	})

	t.Run("CONF-I07 write catalog", func(t *testing.T) {
		if catalog == nil {
			t.Skip("protocol declares no catalog route")
		}
		for _, models := range [][]protocol.ModelInfo{nil, {{ID: "model-a"}, {ID: "model-b"}}} {
			rec := httptest.NewRecorder()
			in.WriteCatalog(rec, models)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") == "" {
				t.Errorf("WriteCatalog(%d models): status=%d header=%v", len(models), rec.Code, rec.Header())
			}
			for _, m := range models {
				if !strings.Contains(rec.Body.String(), `"`+m.ID+`"`) {
					t.Errorf("WriteCatalog body missing %s: %s", m.ID, rec.Body.String())
				}
			}
		}
	})
}

// parseVia 通过真实的 http.ServeMux 调用 Parse，这样路径通配段（r.PathValue）与生产环境一致。
func parseVia(t *testing.T, in protocol.Ingress, rt protocol.Route, c IngressCase) (*protocol.Envelope, error) {
	t.Helper()
	var (
		env    *protocol.Envelope
		err    error
		called bool
	)
	mux := http.NewServeMux()
	mux.HandleFunc(rt.Method+" "+rt.Path, func(_ http.ResponseWriter, r *http.Request) {
		called = true
		var body []byte
		if rt.Body == protocol.BodyBuffered {
			body = c.Body
		}
		env, err = in.Parse(r, rt, body)
	})
	path := c.Path
	if path == "" {
		path = rt.Path
	}
	if c.Query != "" {
		path += "?" + c.Query
	}
	req := httptest.NewRequest(rt.Method, path, bytes.NewReader(c.Body))
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if len(c.Body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatalf("%s: request path %q does not match route %s %s", c.Name, path, rt.Method, rt.Path)
	}
	return env, err
}

func checkEnvelope(t *testing.T, in protocol.Ingress, rt protocol.Route, c IngressCase, env *protocol.Envelope) {
	t.Helper()
	wantModel := c.WantModel
	if wantModel == "" && !c.WantNoModel {
		wantModel = SuiteModel
	}
	if env.Model == "" && !env.Affinity.Strict {
		t.Errorf("%s: Envelope.Model is empty without strict affinity (IN-G03)", c.Name)
	}
	if env.Protocol != in.Protocol() || env.Op != rt.Op {
		t.Errorf("%s: Protocol/Op = %q/%q, want %q/%q", c.Name, env.Protocol, env.Op, in.Protocol(), rt.Op)
	}
	if env.Model != wantModel {
		t.Errorf("%s: Model = %q, want %q", c.Name, env.Model, wantModel)
	}
	if env.Stream != c.WantStream {
		t.Errorf("%s: Stream = %v, want %v", c.Name, env.Stream, c.WantStream)
	}
	if rt.Body == protocol.BodyBuffered && !bytes.Equal(env.Body, c.Body) {
		t.Errorf("%s: Envelope.Body differs from the request body", c.Name)
	}
	if rt.Body != protocol.BodyBuffered && env.Body != nil {
		t.Errorf("%s: Envelope.Body must be nil for non-buffered routes", c.Name)
	}
	if env.BodyStream != nil {
		t.Errorf("%s: Envelope.BodyStream is set by the kernel, not by Parse", c.Name)
	}
	if env.Header == nil {
		t.Errorf("%s: Envelope.Header is nil", c.Name)
	}
	for unit := range env.Estimate {
		if !pricing.ValidUnit(unit) {
			t.Errorf("%s: Estimate has invalid unit %q", c.Name, unit)
		}
	}
	if c.WantEstimate != nil && !env.Estimate.Equal(c.WantEstimate) {
		t.Errorf("%s: Estimate = %v, want %v", c.Name, env.Estimate, c.WantEstimate)
	}
	for k, v := range c.WantParams {
		if env.Params[k] != v {
			t.Errorf("%s: Params[%q] = %q, want %q", c.Name, k, env.Params[k], v)
		}
	}
	if env.Affinity.Strict != c.WantStrict {
		t.Errorf("%s: Affinity.Strict = %v, want %v", c.Name, env.Affinity.Strict, c.WantStrict)
	}
	if env.Affinity.Strict && (env.Affinity.Source != protocol.AffinityPin || len(env.Affinity.Value) == 0) {
		t.Errorf("%s: strict affinity must use AffinityPin with a value, got %+v", c.Name, env.Affinity)
	}
}

// ---------------------------------------------------------------- Egress

// EgressCase 是一次出站尝试的样例：一个请求信封 + 一个脚本化的假上游 + 期望结果。
type EgressCase struct {
	Name string
	// Envelope 是同协议透传时的入站信封（Protocol 由套件填写；套件还会在 Header 中加入客户端凭据，用于检查隔离）。
	// Envelope.BodyStream 被忽略：流式请求体用 NewBodyStream 提供。
	Envelope protocol.Envelope
	// NewBodyStream 为 BodyStream 路由的样例提供请求体；套件每次尝试调用一次（流只能读一次）。nil = 没有流式请求体。
	NewBodyStream func() io.Reader
	// Upstream 是假上游，收到的请求路径已去掉 UpstreamPrefix。一次尝试中可能被调用多次（如提交 + 轮询）。
	Upstream http.Handler
	// CheckUpstream 对每个上游请求调用（可选），用于检查改写后的请求体等协议细节。
	CheckUpstream func(t *testing.T, r *http.Request, body []byte)

	WantKind   protocol.ResponseKind
	WantUsage  pricing.Usage
	WantSource protocol.UsageSource
	WantPins   []string // nil = 期望没有
	WantFault  bool
	// WantClient 是客户端最终收到的字节（Buffered：Body；Raw：全部字节；Events：各事件 Data 以 "\n" 连接）。nil = 不检查。
	WantClient []byte
}

// EgressFixture 是出站适配器的样例。
type EgressFixture struct {
	Cases []EgressCase // 至少 1 个
	// InvalidRequests 是 Prepare 必须拒绝为 invalid_request 的信封；协议对请求体无要求时可以为空。
	InvalidRequests []protocol.Envelope
}

// RunEgress 对出站适配器执行一致性检查（CONF-E01 ~ CONF-E06）。
func RunEgress(t *testing.T, eg protocol.Egress, fx EgressFixture) {
	t.Helper()
	if len(fx.Cases) == 0 {
		t.Fatal("EgressFixture has no cases")
	}
	units := map[string]bool{}
	t.Run("CONF-E01 units", func(t *testing.T) {
		for _, u := range eg.Units() {
			if !pricing.ValidUnit(u) {
				t.Errorf("invalid unit %q", u)
			}
			units[u] = true
		}
		for _, c := range fx.Cases {
			for u := range c.WantUsage {
				if !units[u] {
					t.Errorf("%s: unit %q is reported but not declared in Units()", c.Name, u)
				}
			}
		}
	})

	t.Run("CONF-E02 prepare rejects invalid requests", func(t *testing.T) {
		for i, env := range fx.InvalidRequests {
			env.Protocol = eg.Protocol()
			if _, err := eg.Prepare(&env, SuiteUpstreamModel); apperr.CodeOf(err) != apperr.CodeInvalidRequest {
				t.Errorf("InvalidRequests[%d]: Prepare() error = %v, want invalid_request", i, err)
			}
		}
	})

	for _, c := range fx.Cases {
		t.Run("CONF-E03 attempt "+c.Name, func(t *testing.T) {
			resp := runAttempt(t, eg, c, c.Upstream)
			if resp.Kind != c.WantKind {
				t.Fatalf("Kind = %d, want %d", resp.Kind, c.WantKind)
			}
			if resp.Meter == nil {
				t.Fatal("Response.Meter is nil")
			}
			if resp.Kind != protocol.ResponseEvents && resp.Header.Get("Content-Type") == "" {
				t.Error("Response.Header has no Content-Type")
			}
			if resp.Header.Get("Set-Cookie") != "" {
				t.Error("upstream Set-Cookie forwarded to client (FWD-04)")
			}
			got := drain(t, resp)
			if c.WantClient != nil && !bytes.Equal(got, c.WantClient) {
				t.Errorf("client bytes =\n%s\nwant\n%s", got, c.WantClient)
			}
			u, src := resp.Meter.Usage()
			if src != c.WantSource || !u.Equal(c.WantUsage) {
				t.Errorf("Meter.Usage() = %v, %v; want %v, %v", u, src, c.WantUsage, c.WantSource)
			}
			if _, fault := resp.Meter.Fault(); fault != c.WantFault {
				t.Errorf("Meter.Fault() = %v, want %v", fault, c.WantFault)
			}
			if !reflect.DeepEqual(resp.Pins, c.WantPins) {
				t.Errorf("Pins = %v, want %v", resp.Pins, c.WantPins)
			}
		})
	}

	t.Run("CONF-E04 upstream errors become AttemptError", func(t *testing.T) {
		c := fx.Cases[0]
		for _, status := range []int{400, 401, 403, 429, 500, 503} {
			errUp := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"suite upstream error"}`)
			})
			_, err := doOnce(t, eg, c, errUp)
			var ae *protocol.AttemptError
			if !errors.As(err, &ae) {
				t.Errorf("status %d: Do() error = %v, want *AttemptError", status, err)
				continue
			}
			want := protocol.ClassifyHTTP(status, http.Header{"Retry-After": {"2"}}, time.Now())
			if ae.Outcome.Kind != want.Kind || ae.Outcome.Status != status {
				t.Errorf("status %d: Outcome = %+v, want kind %s", status, ae.Outcome, want.Kind)
			}
			if status == 429 && ae.Outcome.RetryAfter != 2*time.Second {
				t.Errorf("status 429: RetryAfter = %v, want 2s", ae.Outcome.RetryAfter)
			}
			if status == 400 && !bytes.Contains(ae.Body, []byte("suite upstream error")) {
				t.Errorf("status 400: AttemptError.Body = %q, want upstream error body", ae.Body)
			}
		}
	})

	t.Run("CONF-E05 meters are independent", func(t *testing.T) {
		c := fx.Cases[0]
		a := runAttempt(t, eg, c, c.Upstream)
		drain(t, a)
		ua, _ := a.Meter.Usage()
		b := runAttempt(t, eg, c, c.Upstream)
		drain(t, b)
		ub, _ := b.Meter.Usage()
		if ua2, _ := a.Meter.Usage(); !ua.Equal(ub) || !ua.Equal(ua2) {
			t.Errorf("usage differs between attempts (shared state?): %v vs %v (first now %v)", ua, ub, ua2)
		}
	})

	t.Run("CONF-E06 prepare does not modify the envelope body", func(t *testing.T) {
		for _, c := range fx.Cases {
			env := c.Envelope
			orig := append([]byte(nil), env.Body...)
			env.Protocol = eg.Protocol()
			env.BodyStream = nil
			if c.NewBodyStream != nil {
				env.BodyStream = c.NewBodyStream()
			}
			env.Header = http.Header{}
			if _, err := eg.Prepare(&env, SuiteUpstreamModel); err != nil {
				t.Errorf("%s: Prepare() error = %v", c.Name, err)
			}
			if !bytes.Equal(orig, env.Body) {
				t.Errorf("%s: Prepare modified Envelope.Body in place", c.Name)
			}
		}
	})
}

// runAttempt 执行一次 Prepare + Do；失败时立即终止当前测试。
func runAttempt(t *testing.T, eg protocol.Egress, c EgressCase, up http.Handler) *protocol.Response {
	t.Helper()
	resp, err := doOnce(t, eg, c, up)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	return resp
}

func doOnce(t *testing.T, eg protocol.Egress, c EgressCase, up http.Handler) (*protocol.Response, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		checkUpstreamRequest(t, r)
		if c.CheckUpstream != nil {
			c.CheckUpstream(t, r, body)
		}
		w.Header().Set("Set-Cookie", "upstream_session=secret")
		r.Body = io.NopCloser(bytes.NewReader(body))
		http.StripPrefix(UpstreamPrefix, up).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	env := c.Envelope
	env.Protocol = eg.Protocol()
	env.BodyStream = nil
	if c.NewBodyStream != nil {
		env.BodyStream = c.NewBodyStream()
	}
	env.Header = env.Header.Clone()
	if env.Header == nil {
		env.Header = http.Header{}
	}
	env.Header.Set("Authorization", "Bearer "+clientToken)
	env.Header.Set("x-api-key", clientToken)
	env.Header.Set("x-goog-api-key", clientToken)
	env.Header.Set("Cookie", "session="+clientToken)
	env.Header.Set("X-Forwarded-For", "203.0.113.1")
	call, err := eg.Prepare(&env, SuiteUpstreamModel)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return eg.Do(ctx, protocol.Upstream{
		BaseURL:          srv.URL + UpstreamPrefix + "/",
		Secret:           []byte(upstreamSecret),
		Client:           srv.Client(),
		MaxBufferedBytes: 1 << 20,
	}, call)
}

// checkUpstreamRequest 实现 HDR-01、HDR-02 的通用部分：路径在 base_url 之下、不泄露客户端凭据、带上游凭据。
func checkUpstreamRequest(t *testing.T, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, UpstreamPrefix+"/") || strings.Contains(r.URL.Path, "//") {
		t.Errorf("upstream path %q is not under the base URL path", r.URL.Path)
	}
	secretSeen := strings.Contains(r.URL.RawQuery, upstreamSecret)
	if strings.Contains(r.URL.String(), clientToken) {
		t.Error("client token leaked upstream in URL")
	}
	for name, vals := range r.Header {
		for _, v := range vals {
			if strings.Contains(v, clientToken) {
				t.Errorf("client token leaked upstream in header %s", name)
			}
			if strings.Contains(v, upstreamSecret) {
				secretSeen = true
			}
		}
	}
	if r.Header.Get("X-Forwarded-For") != "" {
		t.Error("client X-Forwarded-For forwarded upstream")
	}
	if !secretSeen {
		t.Error("upstream secret not present in any request header or the query string")
	}
}

// drain 读完响应并返回客户端会收到的字节（Events：各事件 Data 以 "\n" 连接）。
func drain(t *testing.T, resp *protocol.Response) []byte {
	t.Helper()
	switch resp.Kind {
	case protocol.ResponseBuffered:
		return resp.Body
	case protocol.ResponseRaw:
		defer resp.Raw.Close()
		b, err := io.ReadAll(resp.Raw)
		if err != nil {
			t.Errorf("reading Raw: %v", err)
		}
		return b
	case protocol.ResponseEvents:
		defer resp.Events.Close()
		var parts [][]byte
		for {
			ev, err := resp.Events.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Errorf("Events.Next() error = %v", err)
				break
			}
			parts = append(parts, ev.Data)
		}
		return bytes.Join(parts, []byte("\n"))
	}
	t.Fatalf("unknown response kind %d", resp.Kind)
	return nil
}

// ---------------------------------------------------------------- 假上游与断言工具

// ServeFile 返回以 contentType 输出文件内容的假上游（例如录制的 .sse 流）。
func ServeFile(contentType, path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(b)
	})
}

// ServeBody 返回以 contentType 输出固定内容的假上游。
func ServeBody(contentType string, body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	})
}

// AssertTopLevelPreserved 断言 out 与 in 两个 JSON 对象中，除 except 外的每个顶层键的值字节完全相同（PT-01）。
// 用于 JSON 协议的 EgressCase.CheckUpstream。
func AssertTopLevelPreserved(t *testing.T, in, out []byte, except ...string) {
	t.Helper()
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal(in, &a); err != nil {
		t.Fatalf("input is not a JSON object: %v", err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatalf("upstream body is not a JSON object: %v: %s", err, out)
	}
	skip := map[string]bool{}
	for _, k := range except {
		skip[k] = true
	}
	for k, v := range a {
		if !skip[k] && !bytes.Equal(bytes.TrimSpace(v), bytes.TrimSpace(b[k])) {
			t.Errorf("top-level %q changed:\n in: %s\nout: %s", k, v, b[k])
		}
	}
}
