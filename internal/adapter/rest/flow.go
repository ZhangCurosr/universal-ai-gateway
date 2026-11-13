package rest

// 声明式多步请求（spec/08 §8.3.7，任务卡 T67）：同步等待（提交 → 轮询 → 取结果）与预签名上传。
// 虚拟端点（EndpointDef.Flow 非 nil）不转发到上游同名路径，而是在一次尝试内依次执行这些步骤；
// 全部步骤使用同一个 up.Secret（EG-05）。

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// FlowDef 是虚拟端点的多步请求（FL-01）。Submit 与 Upload 恰好一个非 nil。
type FlowDef struct {
	Submit *FlowStep
	Upload *FlowUpload
	Poll   *FlowPoll
	Result *FlowResult
}

// FlowStep 是一步上游请求：方法、上游路径（已转义、以 / 开头）与任务 ID 的字段路径。
type FlowStep struct {
	Method string
	Path   string
	TaskID []string // nil = 不取任务 ID
}

// FlowUpload 是预签名上传（FL-03、FL-04）。
type FlowUpload struct {
	FlowStep
	Body      []BodyField
	UploadURL []string
}

// BodyField 是申请上传地址请求体中的一个字段（FL-04）。
type BodyField struct {
	Target   []string
	Query    string
	Type     string // string、bool、int、list
	Required bool
}

// FlowPoll 是轮询（FL-05）。Path 中的 "{task_id}" 在发送时替换为转义后的任务 ID。
type FlowPoll struct {
	Method  string
	Path    string
	State   []string
	Done    []string
	Failed  []string
	Timeout time.Duration // 0 = 10 分钟
}

// FlowResult 是同步结果（FL-06）。
type FlowResult struct {
	URL         []string // nil = 返回完成时的查询响应
	ZipEntry    string
	ContentType string
}

const (
	flowPollInitial = 2 * time.Second
	flowPollMax     = 15 * time.Second
	flowPollTimeout = 10 * time.Minute
	defaultBufLimit = 64 << 20
)

// validateFlowDef 在构造时检查 flow 是否可执行（例如 body 的数组下标从 0 依次出现）。
func validateFlowDef(service string, f *FlowDef) error {
	if (f.Submit == nil) == (f.Upload == nil) {
		return fmt.Errorf("rest: service %q: a flow needs exactly one of submit or upload", service)
	}
	if f.Poll != nil && !strings.Contains(f.Poll.Path, "{task_id}") {
		return fmt.Errorf("rest: service %q: poll path must contain {task_id}", service)
	}
	if u := f.Upload; u != nil {
		q := url.Values{}
		for _, b := range u.Body {
			switch b.Type {
			case "bool":
				q.Set(b.Query, "true")
			case "int":
				q.Set(b.Query, "1")
			default:
				q.Set(b.Query, "x")
			}
		}
		if _, err := buildBody(u.Body, q); err != nil {
			return fmt.Errorf("rest: service %q: upload body: %s", service, err.Message)
		}
	}
	return nil
}

// buildBody 按 FL-04 用查询参数组成 JSON 对象。错误是 400，消息指明参数名。
func buildBody(fields []BodyField, q url.Values) (map[string]any, *apperr.Error) {
	sorted := append([]BodyField(nil), fields...)
	sort.SliceStable(sorted, func(i, j int) bool { return strings.Join(sorted[i].Target, ".") < strings.Join(sorted[j].Target, ".") })
	var root any = map[string]any{}
	for _, f := range sorted {
		if !q.Has(f.Query) {
			if f.Required {
				return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("query parameter %q is required", f.Query))
			}
			continue
		}
		raw := q.Get(f.Query)
		var v any
		switch f.Type {
		case "bool":
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("query parameter %q must be true or false", f.Query))
			}
			v = b
		case "int":
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("query parameter %q must be an integer", f.Query))
			}
			v = n
		case "list":
			items := []any{}
			for _, s := range strings.Split(raw, ",") {
				if s = strings.TrimSpace(s); s != "" {
					items = append(items, s)
				}
			}
			v = items
		default:
			v = raw
		}
		next, err := setIn(root, f.Target, v)
		if err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("query parameter %q: %v", f.Query, err))
		}
		root = next
	}
	m, _ := root.(map[string]any) // 根总是对象
	return m, nil
}

// setIn 把 val 写到 cur 中 segs 指定的位置；数字段表示数组下标，只能从 0 依次出现。
func setIn(cur any, segs []string, val any) (any, error) {
	if len(segs) == 0 {
		return val, nil
	}
	seg := segs[0]
	if isIndex(seg) {
		idx, _ := strconv.Atoi(seg) // isIndex 已保证是十进制数字
		arr, ok := cur.([]any)
		if cur != nil && !ok {
			return nil, errors.New("field path conflicts with an existing object")
		}
		if idx > len(arr) {
			return nil, fmt.Errorf("array index %d skips earlier elements", idx)
		}
		var child any
		if idx < len(arr) {
			child = arr[idx]
		}
		nv, err := setIn(child, segs[1:], val)
		if err != nil {
			return nil, err
		}
		if idx == len(arr) {
			return append(arr, nv), nil
		}
		arr[idx] = nv
		return arr, nil
	}
	m, ok := cur.(map[string]any)
	if cur != nil && !ok {
		return nil, errors.New("field path conflicts with an existing value")
	}
	if m == nil {
		m = map[string]any{}
	}
	nv, err := setIn(m[seg], segs[1:], val)
	if err != nil {
		return nil, err
	}
	m[seg] = nv
	return m, nil
}

func isIndex(s string) bool {
	if s == "" || len(s) > 6 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// flowUsage 累积 flow 中见到的用量（FL-09）。
type flowUsage struct {
	ep     *EndpointDef
	reqDoc any
	query  url.Values
	seen   map[string]int64
	warned map[string]bool
}

// observe 对一个上游 JSON 响应求值 json 来源；后见到的值覆盖先见到的。
func (a *flowUsage) observe(doc any) []string {
	var bad []string
	for unit, r := range a.ep.Usage {
		if r.JSON == nil || doc == nil {
			continue
		}
		v, ok := lookupPath(doc, r.JSON)
		if !ok {
			continue
		}
		if n, ok := nonNegInt(v); ok {
			a.seen[unit] = n
		} else if !a.warned[unit] {
			a.warned[unit] = true
			bad = append(bad, unit)
		}
	}
	return bad
}

// observeZip 对结果包求值 zip_json 来源（FL-09a）；轮询中已经取到的单位不覆盖。返回取值不合法的单位。
func (a *flowUsage) observeZip(zr *zip.Reader, limit int64) []string {
	var bad []string
	for unit, r := range a.ep.Usage {
		z := r.ZipJSON
		if z == nil {
			continue
		}
		if _, ok := a.seen[unit]; ok {
			continue
		}
		raw, ok := zipEntry(zr, z.Entry, limit)
		if !ok {
			continue
		}
		doc, ok := decodeJSON(raw)
		if !ok {
			bad = append(bad, unit)
			continue
		}
		v, ok := lookupPath(doc, z.Count)
		if !ok {
			continue
		}
		if arr, isArr := v.([]any); isArr {
			a.seen[unit] = int64(len(arr))
		} else if n, ok := nonNegInt(v); ok {
			a.seen[unit] = n
		} else {
			bad = append(bad, unit)
		}
	}
	return bad
}

// zipEntry 读取第一个文件名（不含目录）等于 name 的文件，至多 limit 字节；找不到或超限返回 ok=false。
func zipEntry(zr *zip.Reader, name string, limit int64) ([]byte, bool) {
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() || path.Base(zf.Name) != name {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(rc, limit+1))
		_ = rc.Close() // 只读，关闭失败无可处理
		if err != nil || int64(len(b)) > limit {
			return nil, false
		}
		return b, true
	}
	return nil, false
}

func (a *flowUsage) meter() *meter {
	if a.ep.Usage == nil {
		return &meter{usage: pricing.Usage{UnitRequests: 1}, src: protocol.UsageUpstream}
	}
	if len(a.ep.Usage) == 0 {
		return &meter{src: protocol.UsageUpstream}
	}
	u := pricing.Usage{}
	estimated := false
	for unit, r := range a.ep.Usage {
		if n, ok := a.seen[unit]; ok {
			u[unit] = n
			continue
		}
		if n, ok, _ := requestValue(r, a.reqDoc, a.query); ok {
			u[unit] = n
			estimated = true
			continue
		}
		if r.Default != nil {
			u[unit] = *r.Default
			estimated = true
		}
	}
	switch {
	case len(u) == 0:
		return &meter{src: protocol.UsageNone}
	case estimated:
		return &meter{usage: u, src: protocol.UsageEstimated}
	}
	return &meter{usage: u, src: protocol.UsageUpstream}
}

// afterPaid 把第一步成功之后的失败改写为不可重试、不惩罚 Key（FL-08）。
func afterPaid(err error) error {
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) {
		ae = &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError, Status: http.StatusBadGateway}, Err: err}
	}
	if !ae.Outcome.Kind.Retryable() {
		return ae
	}
	out := *ae
	out.Outcome.Kind = protocol.OutcomeLocalError
	out.Err = fmt.Errorf("rest flow: task created upstream but not completed (%s): %w", ae.Outcome.Kind, ae.Err)
	return &out
}

func badUpstream(msg string) *protocol.AttemptError {
	return &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError, Status: http.StatusBadGateway}, Err: errors.New("rest flow: " + msg)}
}

// allowedURL 检查上传与下载地址（FL-03、FL-06）：必须是 https，或与 base_url 同源。
func allowedURL(raw, baseURL string) (target string, sameOrigin, ok bool) {
	u, err := url.Parse(raw)
	base, berr := url.Parse(baseURL)
	if err != nil || berr != nil || u.Host == "" {
		return "", false, false
	}
	sameOrigin = u.Scheme == base.Scheme && u.Host == base.Host
	if u.Scheme != "https" && !sameOrigin {
		return "", false, false
	}
	return u.String(), sameOrigin, true
}

// applyKey 把上游 Key 放进请求（§8.3.3）。
func applyKey(req *http.Request, k *parsedSecret) {
	if k.headerName != "" {
		req.Header.Set(k.headerName, k.headerValue)
	}
	if k.queryParam != "" {
		q := stripQueryParam(req.URL.RawQuery, k.queryParam)
		p := k.queryParam + "=" + url.QueryEscape(k.queryValue)
		if q == "" {
			req.URL.RawQuery = p
		} else {
			req.URL.RawQuery = q + "&" + p
		}
	}
}

func (e Egress) wait(ctx context.Context, d time.Duration) error {
	if e.sleep != nil {
		return e.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// flowJSON 发送 flow 中的一步并读取 JSON 响应；错误规则对它同样生效（FL-08）。
func (e Egress) flowJSON(ctx context.Context, up protocol.Upstream, key *parsedSecret, st *callState, method, upPath, rawQuery string, body []byte) (any, []byte, error) {
	target := protocol.JoinURL(up.BaseURL, upPath)
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", e.userAgent())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	applyKey(req, key)
	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, nil, e.classifyError(ctx, st, err, req.URL.Host, req.URL.Path, up.Secret)
	}
	raw, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, nil, err
	}
	doc, isJSON := decodeJSON(raw)
	if rule := st.def.matchError(resp.StatusCode, doc, isJSON); rule != nil {
		o := protocol.Outcome{Status: resp.StatusCode}
		rule.apply(&o, e.clock())
		eb := raw
		if len(eb) > protocol.MaxErrorBodyBytes {
			eb = eb[:protocol.MaxErrorBodyBytes]
		}
		return nil, nil, &protocol.AttemptError{Outcome: o, Body: eb, Header: http.Header{"Content-Type": {"application/json"}},
			Err: fmt.Errorf("rest flow: upstream %d matched an error rule", resp.StatusCode)}
	}
	if !isJSON {
		return nil, nil, badUpstream("upstream response is not JSON")
	}
	return doc, raw, nil
}

// doFlow 执行虚拟端点（FL-01 ~ FL-11）。
func (e Egress) doFlow(ctx context.Context, up protocol.Upstream, call *protocol.Call, st *callState, key *parsedSecret) (*protocol.Response, error) {
	f := st.ep.Flow
	query, _ := url.ParseQuery(call.Params["query"]) // 解析失败时视为没有查询参数
	acc := &flowUsage{ep: st.ep, query: query, seen: map[string]int64{}, warned: map[string]bool{}}
	if len(call.Body) > 0 {
		acc.reqDoc, _ = decodeJSON(call.Body)
	}

	// 第一步：提交或上传。成功之前的失败按结果分类处理（可以换 Key），不计费。
	var first any
	var firstRaw []byte
	var taskPath []string
	if s := f.Submit; s != nil {
		if call.Body == nil && call.BodyStream != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "this endpoint forwards a JSON request body; call it under /rest-idem/")
		}
		doc, raw, err := e.flowJSON(ctx, up, key, st, s.Method, s.Path, call.Params["query"], call.Body)
		if err != nil {
			return nil, err
		}
		first, firstRaw, taskPath = doc, raw, s.TaskID
	} else {
		doc, err := e.flowUpload(ctx, up, call, st, key, query)
		if err != nil {
			return nil, err
		}
		first, taskPath = doc, f.Upload.TaskID
	}

	var pins []string
	taskID := ""
	if taskPath != nil {
		id, ok := taskIDFrom(first, taskPath)
		if !ok {
			e.logger().WarnContext(ctx, "rest flow: task id not found in response", "service", st.service)
			if f.Poll != nil {
				return &protocol.Response{Meter: acc.meter()}, afterPaid(badUpstream("task id not found in response"))
			}
		} else {
			taskID = id
			pins = []string{pinValue(st.service, id)}
		}
	}
	json200 := http.Header{"Content-Type": {"application/json"}}
	if f.Poll == nil { // 提交后立即返回（FL-01、FL-09）
		e.warnUnits(ctx, st, acc.observe(first))
		body := firstRaw
		if f.Upload != nil { // 申请地址的响应含签名上传地址：不交给客户端
			body, _ = json.Marshal(map[string]string{"task_id": taskID}) // 只含字符串，必然可以序列化
		}
		return &protocol.Response{Kind: protocol.ResponseBuffered, Status: http.StatusOK, Header: json200, Body: body, Meter: acc.meter(), Pins: pins}, nil
	}

	// 第一步已成功：任务已在上游创建。之后的失败不重试、保留计量（FL-08、CORE-12）。
	done, doneRaw, err := e.flowPoll(ctx, up, key, st, f.Poll, taskID, acc)
	if err != nil {
		return &protocol.Response{Meter: acc.meter()}, afterPaid(err)
	}
	if f.Result == nil || f.Result.URL == nil {
		return &protocol.Response{Kind: protocol.ResponseBuffered, Status: http.StatusOK, Header: json200, Body: doneRaw, Meter: acc.meter(), Pins: pins}, nil
	}
	resp, err := e.flowResult(ctx, up, key, f.Result, done, acc, st)
	if err != nil {
		return &protocol.Response{Meter: acc.meter()}, afterPaid(err)
	}
	resp.Meter, resp.Pins = acc.meter(), pins
	return resp, nil
}

// flowUpload 申请上传地址并把客户端请求体 PUT 过去（FL-03、FL-04）。返回申请地址的响应。
func (e Egress) flowUpload(ctx context.Context, up protocol.Upstream, call *protocol.Call, st *callState, key *parsedSecret, query url.Values) (any, error) {
	u := st.ep.Flow.Upload
	if call.BodyStream == nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "this endpoint uploads the request body as a file; call it under /rest/ with POST or PATCH")
	}
	size, err := strconv.ParseInt(call.Params["size"], 10, 64)
	if err != nil || size <= 0 {
		return nil, apperr.New(apperr.CodeInvalidRequest, "Content-Length is required (chunked uploads are not supported)")
	}
	body, aerr := buildBody(u.Body, query)
	if aerr != nil {
		return nil, aerr
	}
	raw, _ := json.Marshal(body) // 只含字符串、布尔、整数与数组，必然可以序列化
	doc, _, err := e.flowJSON(ctx, up, key, st, u.Method, u.Path, "", raw)
	if err != nil {
		return nil, err
	}
	v, ok := lookupPath(doc, u.UploadURL)
	s, isStr := v.(string)
	if !ok || !isStr || s == "" {
		return nil, badUpstream("upload url not found in response")
	}
	dest, same, ok := allowedURL(s, up.BaseURL)
	if !ok {
		return nil, badUpstream("upload url must be https or on the upstream origin")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, dest, call.BodyStream)
	if err != nil {
		return nil, badUpstream("invalid upload url")
	}
	req.ContentLength = size
	req.Header.Set("User-Agent", e.userAgent())
	if same { // 第三方存储不带上游 Key
		applyKey(req, key)
	}
	resp, err := protocol.Send(up, req)
	if err != nil {
		var ae *protocol.AttemptError
		if errors.As(err, &ae) {
			sanitizeAttemptErrorURL(ae)
			ae.Body, ae.Header = nil, nil // 存储的错误体可能含签名信息
			if ae.Outcome.Kind == protocol.OutcomeAuthFailed || ae.Outcome.Kind == protocol.OutcomePermissionDenied {
				ae.Outcome.Kind = protocol.OutcomeServerError // 签名地址被拒不是 Key 的问题
			}
			return nil, ae
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			sanitizeURLError(ue)
		}
		return nil, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, protocol.MaxErrorBodyBytes)) // 尽量读完以复用连接
	_ = resp.Body.Close()                                                             // 清理调用，错误无意义
	return doc, nil
}

// flowPoll 轮询直到完成（FL-05）。返回完成时的响应。
func (e Egress) flowPoll(ctx context.Context, up protocol.Upstream, key *parsedSecret, st *callState, p *FlowPoll, taskID string, acc *flowUsage) (any, []byte, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = flowPollTimeout
	}
	deadline := e.clock().Add(timeout)
	upPath := strings.Replace(p.Path, "{task_id}", url.PathEscape(taskID), 1)
	interval := flowPollInitial
	for {
		if err := e.wait(ctx, interval); err != nil {
			return nil, nil, &protocol.AttemptError{Outcome: protocol.ClassifyNetError(err, ctx), Err: err}
		}
		doc, raw, err := e.flowJSON(ctx, up, key, st, p.Method, upPath, "", nil)
		if err != nil {
			return nil, nil, err
		}
		e.warnUnits(ctx, st, acc.observe(doc))
		v, _ := lookupPath(doc, p.State)
		state := valueText(v)
		if containsString(p.Done, state) {
			return doc, raw, nil
		}
		if containsString(p.Failed, state) {
			eb := raw
			if len(eb) > protocol.MaxErrorBodyBytes {
				eb = eb[:protocol.MaxErrorBodyBytes]
			}
			return nil, nil, &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeClientError, Status: http.StatusBadRequest},
				Body: eb, Header: http.Header{"Content-Type": {"application/json"}}, Err: errors.New("rest flow: upstream task failed")}
		}
		if !e.clock().Before(deadline) {
			return nil, nil, &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeTimeout, Status: http.StatusGatewayTimeout},
				Err: fmt.Errorf("rest flow: task not finished within %s", timeout)}
		}
		interval = min(interval*3/2, flowPollMax)
	}
}

// flowResult 下载结果（FL-06、FL-07）。
func (e Egress) flowResult(ctx context.Context, up protocol.Upstream, key *parsedSecret, r *FlowResult, done any, acc *flowUsage, st *callState) (*protocol.Response, error) {
	v, ok := lookupPath(done, r.URL)
	s, isStr := v.(string)
	if !ok || !isStr || s == "" {
		return nil, badUpstream("result url not found in response")
	}
	dest, same, ok := allowedURL(s, up.BaseURL)
	if !ok {
		return nil, badUpstream("result url must be https or on the upstream origin")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dest, nil)
	if err != nil {
		return nil, badUpstream("invalid result url")
	}
	req.Header.Set("User-Agent", e.userAgent())
	if same {
		applyKey(req, key)
	}
	resp, err := protocol.Send(up, req)
	if err != nil {
		var ae *protocol.AttemptError
		if errors.As(err, &ae) {
			sanitizeAttemptErrorURL(ae)
			ae.Body, ae.Header = nil, nil
			ae.Outcome.Kind = protocol.OutcomeServerError // 结果地址不可用不是 Key 的问题
			ae.Outcome.Status = http.StatusBadGateway
			return nil, ae
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			sanitizeURLError(ue)
		}
		return nil, err
	}
	ct := r.ContentType
	if r.ZipEntry == "" {
		if ct == "" {
			ct = resp.Header.Get("Content-Type")
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		return &protocol.Response{Kind: protocol.ResponseRaw, Status: http.StatusOK, Header: http.Header{"Content-Type": {ct}}, Raw: resp.Body}, nil
	}
	limit := up.MaxBufferedBytes
	if limit <= 0 {
		limit = defaultBufLimit
	}
	data, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, badUpstream("result is not a zip archive")
	}
	e.warnUnits(ctx, st, acc.observeZip(zr, limit))
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() || path.Base(zf.Name) != r.ZipEntry {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, badUpstream("cannot open " + r.ZipEntry + " in the result archive")
		}
		content, err := io.ReadAll(io.LimitReader(rc, limit+1))
		_ = rc.Close() // 只读，关闭失败无可处理
		if err != nil {
			return nil, badUpstream("cannot read " + r.ZipEntry + " in the result archive")
		}
		if int64(len(content)) > limit {
			return nil, &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeLocalError, Status: http.StatusBadGateway}, Err: protocol.ErrResponseTooLarge}
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		return &protocol.Response{Kind: protocol.ResponseBuffered, Status: http.StatusOK, Header: http.Header{"Content-Type": {ct}}, Body: content}, nil
	}
	return nil, badUpstream(r.ZipEntry + " not found in the result archive")
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (e Egress) warnUnits(ctx context.Context, st *callState, units []string) {
	for _, unit := range units {
		e.logger().WarnContext(ctx, "rest: usage value is not a non-negative integer; ignored", "service", st.service, "unit", unit)
	}
}
