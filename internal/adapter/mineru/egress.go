package mineru

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// Egress 是 MinerU 出站适配器（网关 → MinerU 上游）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
	// InitialPollInterval 轮询起始间隔；<= 0 时默认 1 秒（EG-02，测试可调小）。
	InitialPollInterval time.Duration
	// MaxPollInterval 轮询最大间隔；<= 0 时默认 10 秒（EG-02）。
	MaxPollInterval time.Duration
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回 MinerU 可能报告的计量单位（EG-01，spec/08 §8.2）。
func (Egress) Units() []string {
	return []string{UnitPages, UnitRequests}
}

// Prepare 实现同协议透传与参数校验（spec/08 §8.2）。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	switch env.Op {
	case OpParse, OpTaskCreate, OpTaskGet, OpTaskResult:
	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "unsupported operation: "+env.Op)
	}

	if env.Op == OpTaskGet || env.Op == OpTaskResult {
		if env.Params == nil || strings.TrimSpace(env.Params["id"]) == "" {
			return nil, apperr.New(apperr.CodeInvalidRequest, "task id is required")
		}
	}

	if len(env.Body) > 0 {
		trimmed := bytes.TrimSpace(env.Body)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
		}
	}

	return &protocol.Call{
		Op:         env.Op,
		Model:      upstreamModel,
		Stream:     env.Stream,
		Body:       env.Body,
		BodyStream: env.BodyStream,
		Params:     env.Params,
		Header:     env.Header,
		State:      env.Estimate,
	}, nil
}

// Do 执行一次尝试：根据操作调用 MinerU 上游（spec/08 §3、§8.2）。
func (e Egress) Do(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	switch call.Op {
	case OpTaskCreate:
		return e.doTaskCreate(ctx, up, call)
	case OpTaskGet:
		return e.doTaskGet(ctx, up, call)
	case OpTaskResult:
		return e.doTaskResult(ctx, up, call)
	case OpParse:
		return e.doParse(ctx, up, call)
	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "mineru upstream does not support operation "+call.Op)
	}
}

func (e Egress) doTaskCreate(ctx context.Context, up protocol.Upstream, call *protocol.Call) (result *protocol.Response, err error) {
	reqURL := protocol.JoinURL(up.BaseURL, "/v1/tasks")
	bodyReader := makeBodyReader(call)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	setupRequestHeaders(req, call, up.Secret, e.userAgent())

	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			result = paidSubmission(call, 0)
		}
	}()

	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}

	var pins []string
	if id := extractTaskID(body); id != "" {
		pins = []string{id}
	}

	// spec/08 §8.2: 异步透传提交时按估计页数结算（UsageEstimated）
	est, _ := call.State.(pricing.Usage)
	usage := pricing.Usage{UnitRequests: 1}
	if p := est.Get(UnitPages); p > 0 {
		usage[UnitPages] = p
	}

	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type")
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}

	m := &meter{usage: usage, src: protocol.UsageEstimated}
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: resp.StatusCode,
		Header: h,
		Body:   body,
		Pins:   pins,
		Meter:  m,
	}, nil
}

func (e Egress) doTaskGet(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	id := call.Params["id"]
	reqURL := protocol.JoinURL(up.BaseURL, "/v1/tasks/"+url.PathEscape(id))
	if q := call.Params["query"]; q != "" {
		reqURL += "?" + q
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+string(up.Secret))
	req.Header.Set("User-Agent", e.userAgent())
	req.Header.Set("Accept", "application/json")

	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}

	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}

	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type")
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}

	// spec/08 §8.2: 查询不计费，Meter 返回 (nil, UsageUpstream)
	m := &meter{usage: nil, src: protocol.UsageUpstream}
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: resp.StatusCode,
		Header: h,
		Body:   body,
		Meter:  m,
	}, nil
}

func (e Egress) doTaskResult(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	id := call.Params["id"]
	reqURL := protocol.JoinURL(up.BaseURL, "/v1/tasks/"+url.PathEscape(id)+"/result")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+string(up.Secret))
	req.Header.Set("User-Agent", e.userAgent())
	req.Header.Set("Accept", "*/*")

	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}

	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type", "Content-Length", "Content-Disposition")
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/octet-stream")
	}

	// spec/08 §8.2: 下载不计费，ResponseRaw 流式转发
	m := &meter{usage: nil, src: protocol.UsageUpstream}
	return &protocol.Response{
		Kind:   protocol.ResponseRaw,
		Status: resp.StatusCode,
		Header: h,
		Raw:    resp.Body,
		Meter:  m,
	}, nil
}

func (e Egress) doParse(ctx context.Context, up protocol.Upstream, call *protocol.Call) (result *protocol.Response, err error) {
	// 1. 提交任务到上游 POST /v1/tasks
	submitURL := protocol.JoinURL(up.BaseURL, "/v1/tasks")
	submitReq, err := http.NewRequestWithContext(ctx, http.MethodPost, submitURL, makeBodyReader(call))
	if err != nil {
		return nil, err
	}
	setupRequestHeaders(submitReq, call, up.Secret, e.userAgent())

	submitResp, err := protocol.Send(up, submitReq)
	if err != nil {
		return nil, err
	}
	var actualPages int64
	// CORE-12：提交成功即可能计费；后续读取、轮询、下载失败或取消也保留计量结果。
	defer func() {
		if err != nil {
			result = paidSubmission(call, actualPages)
		}
	}()
	submitBody, err := protocol.ReadBody(up, submitResp)
	if err != nil {
		return nil, err
	}

	taskID := extractTaskID(submitBody)
	if taskID == "" {
		return nil, &protocol.AttemptError{
			Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError, Status: http.StatusBadGateway},
			Body:    submitBody,
			Err:     errors.New("upstream did not return task id"),
		}
	}

	// 2. 轮询任务状态 GET /v1/tasks/{id} 直到完成（EG-02: 1s 起翻倍到 10s，select 等待）
	interval := e.initialPollInterval()
	maxInterval := e.maxPollInterval()

pollLoop:
	for {
		select {
		case <-ctx.Done():
			outcome := protocol.ClassifyNetError(ctx.Err(), ctx)
			return nil, afterSubmit(taskID, &protocol.AttemptError{
				Outcome: outcome,
				Err:     ctx.Err(),
			})
		case <-time.After(interval):
		}

		pollURL := protocol.JoinURL(up.BaseURL, "/v1/tasks/"+url.PathEscape(taskID))
		pollReq, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, afterSubmit(taskID, err)
		}
		pollReq.Header.Set("Authorization", "Bearer "+string(up.Secret))
		pollReq.Header.Set("User-Agent", e.userAgent())
		pollReq.Header.Set("Accept", "application/json")

		pollResp, err := protocol.Send(up, pollReq)
		if err != nil {
			return nil, afterSubmit(taskID, err)
		}
		pollBody, err := protocol.ReadBody(up, pollResp)
		if err != nil {
			return nil, afterSubmit(taskID, err)
		}

		status := extractTaskStatus(pollBody)
		switch status {
		case "completed", "done", "success", "succeeded", "finished":
			actualPages = extractPages(pollBody)
			break pollLoop

		case "failed", "error", "fail", "cancelled", "canceled":
			errMsg := extractErrorMessage(pollBody)
			return nil, &protocol.AttemptError{
				Outcome: protocol.Outcome{Kind: protocol.OutcomeClientError, Status: http.StatusBadRequest},
				Body:    pollBody,
				Err:     errors.New(errMsg),
			}

		default:
			interval = interval * 2
			if interval > maxInterval {
				interval = maxInterval
			}
		}
	}

	// 3. 下载结果 GET /v1/tasks/{id}/result
	resultURL := protocol.JoinURL(up.BaseURL, "/v1/tasks/"+url.PathEscape(taskID)+"/result")
	resReq, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
	if err != nil {
		return nil, afterSubmit(taskID, err)
	}
	resReq.Header.Set("Authorization", "Bearer "+string(up.Secret))
	resReq.Header.Set("User-Agent", e.userAgent())
	resReq.Header.Set("Accept", "*/*")

	resResp, err := protocol.Send(up, resReq)
	if err != nil {
		return nil, afterSubmit(taskID, err)
	}
	resultBody, err := protocol.ReadBody(up, resResp)
	if err != nil {
		return nil, afterSubmit(taskID, err)
	}

	if actualPages == 0 {
		actualPages = extractPages(resultBody)
	}

	h := http.Header{}
	protocol.CopyHeaders(h, resResp.Header, "Content-Type")
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "text/markdown; charset=utf-8")
	}

	// spec/08 §8.2: 同步封装完成时按上游报告的实际页数结算（UsageUpstream）。
	// 上游没报告页数时不能按 0 页结算（少计费），返回 UsageNone 让内核按预留用量结算（spec/06 §3.3）。
	m := &meter{src: protocol.UsageNone}
	if actualPages > 0 {
		m = &meter{usage: pricing.Usage{UnitRequests: 1, UnitPages: actualPages}, src: protocol.UsageUpstream}
	}

	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: resResp.StatusCode,
		Header: h,
		Body:   resultBody,
		Meter:  m,
	}, nil
}

// paidSubmission 不包含可转发的部分结果。页数已知时按实际计费，否则按预留估算。
func paidSubmission(call *protocol.Call, pages int64) *protocol.Response {
	u := pricing.Usage{UnitRequests: 1}
	src := protocol.UsageEstimated
	if pages > 0 {
		u[UnitPages] = pages
		src = protocol.UsageUpstream
	} else if est, ok := call.State.(pricing.Usage); ok {
		if n := est.Get(UnitPages); n > 0 {
			u[UnitPages] = n
		}
	}
	return &protocol.Response{Meter: &meter{usage: u, src: src}}
}

// afterSubmit 把任务提交成功之后的失败改写成不可重试。
//
// spec/08 §8.2：任务一旦创建，上游就会按页计费；提交之后的任何失败都不重试（防止重复付费），
// 错误信息带上任务 ID，便于运维用 GET /v1/tasks/{id} 找回结果。客户端断开保持 Canceled。
func afterSubmit(taskID string, err error) error {
	var ae *protocol.AttemptError
	if !errors.As(err, &ae) {
		ae = &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeNetworkError}, Err: err}
	}
	if !ae.Outcome.Kind.Retryable() {
		return ae
	}
	out := *ae
	out.Outcome.Kind = protocol.OutcomeLocalError // 不可重试且不惩罚 Key
	out.Err = fmt.Errorf("mineru task %s submitted but not completed (%s): %w", taskID, ae.Outcome.Kind, ae.Err)
	return &out
}

func (e Egress) userAgent() string {
	if e.UserAgent != "" {
		return e.UserAgent
	}
	return "universal-ai-gateway"
}

func (e Egress) initialPollInterval() time.Duration {
	if e.InitialPollInterval > 0 {
		return e.InitialPollInterval
	}
	return 1 * time.Second
}

func (e Egress) maxPollInterval() time.Duration {
	if e.MaxPollInterval > 0 {
		return e.MaxPollInterval
	}
	return 10 * time.Second
}

func makeBodyReader(call *protocol.Call) io.Reader {
	if call.BodyStream != nil {
		return call.BodyStream
	}
	if len(call.Body) > 0 {
		return bytes.NewReader(call.Body)
	}
	if u := call.Params["url"]; u != "" {
		b, err := json.Marshal(map[string]string{"url": u})
		if err == nil { // map[string]string 的 Marshal 不会失败
			return bytes.NewReader(b)
		}
	}
	return http.NoBody
}

func setupRequestHeaders(req *http.Request, call *protocol.Call, secret []byte, ua string) {
	req.Header.Set("Authorization", "Bearer "+string(secret))
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")

	if call.Header != nil && call.Header.Get("Content-Type") != "" {
		req.Header.Set("Content-Type", call.Header.Get("Content-Type"))
	} else if (call.Params != nil && call.Params["url"] != "") || bytes.HasPrefix(bytes.TrimSpace(call.Body), []byte("{")) {
		req.Header.Set("Content-Type", "application/json")
	} else if call.BodyStream != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	if call.Header != nil && call.Header.Get("Content-Length") != "" {
		if cl, err := strconv.ParseInt(call.Header.Get("Content-Length"), 10, 64); err == nil && cl > 0 {
			req.ContentLength = cl
		}
	}
}

func extractTaskID(body []byte) string {
	paths := [][]string{
		{"id"},
		{"task_id"},
		{"taskId"},
		{"data", "task_id"},
		{"data", "id"},
		{"data", "taskId"},
	}
	for _, p := range paths {
		if s, ok := jsonx.GetString(body, p...); ok && s != "" {
			return s
		}
	}
	return ""
}

func extractTaskStatus(body []byte) string {
	paths := [][]string{
		{"status"},
		{"state"},
		{"data", "status"},
		{"data", "state"},
		{"task_status"},
	}
	for _, p := range paths {
		if s, ok := jsonx.GetString(body, p...); ok && s != "" {
			return strings.ToLower(strings.TrimSpace(s))
		}
	}
	return ""
}

func extractPages(body []byte) int64 {
	paths := [][]string{
		{"pages"},
		{"page_count"},
		{"data", "pages"},
		{"data", "page_count"},
		{"result", "pages"},
		{"usage", "pages"},
	}
	for _, p := range paths {
		if n, ok := jsonx.GetInt64(body, p...); ok && n > 0 {
			return n
		}
	}
	return 0
}

// meter 报告 MinerU 调用的用量。
type meter struct {
	usage pricing.Usage
	src   protocol.UsageSource
}

func (m *meter) Usage() (pricing.Usage, protocol.UsageSource) {
	return m.usage, m.src
}

func (m *meter) Fault() (protocol.Outcome, bool) {
	return protocol.Outcome{}, false
}
