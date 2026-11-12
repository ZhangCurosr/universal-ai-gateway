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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// 上游 MinerU 精准解析 API v4（spec/08 §8.2.1）。base_url 为站点根，例如 https://mineru.net。
const (
	pathTask      = "/api/v4/extract/task"           // POST 提交；GET pathTask/{id} 查询
	pathUpload    = "/api/v4/file-urls/batch"        // POST 申请文件上传地址（MU-07）
	pathBatch     = "/api/v4/extract-results/batch/" // GET pathBatch{batch_id} 查询上传文件的解析结果
	batchIDPrefix = "batch-"                         // 网关对上传任务的 ID 前缀：batch-<batch_id>（MU-08）
)

// Egress 是 MinerU 出站适配器（网关 → MinerU 上游）。零值可用。
type Egress struct {
	// UserAgent 写入上游请求的 User-Agent（HDR-05）；为空时使用 "universal-ai-gateway"。
	UserAgent string
	// InitialPollInterval 轮询起始间隔；<= 0 时默认 1 秒（EG-02，测试可调小）。
	InitialPollInterval time.Duration
	// MaxPollInterval 轮询最大间隔；<= 0 时默认 10 秒（EG-02）。
	MaxPollInterval time.Duration
	// Now 返回当前时间（计算日配额的冷却时长）；nil 时用 time.Now。
	Now func() time.Time
}

var _ protocol.Egress = Egress{}

// Protocol 返回 Protocol。
func (Egress) Protocol() protocol.Protocol { return Protocol }

// Units 返回 MinerU 可能报告的计量单位（EG-01，spec/08 §8.2）。
func (Egress) Units() []string {
	return []string{UnitPages, UnitRequests}
}

// Prepare 校验操作与参数，并为提交类操作构造上游请求体（MU-02）。
func (Egress) Prepare(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	call := &protocol.Call{
		Op:     env.Op,
		Model:  upstreamModel,
		Stream: env.Stream,
		Params: env.Params,
		Header: env.Header,
		State:  env.Estimate,
	}
	switch env.Op {
	case OpParse, OpTaskCreate:
		body, err := submitBody(env.Body, env.Params["url"])
		if err != nil {
			return nil, err
		}
		call.Body = body
	case OpParseFile, OpTaskUpload:
		body, err := uploadBody(env.Params)
		if err != nil {
			return nil, err
		}
		call.Body = body
		call.BodyStream = env.BodyStream
	case OpTaskGet, OpTaskResult:
		if env.Params == nil || strings.TrimSpace(env.Params["id"]) == "" {
			return nil, apperr.New(apperr.CodeInvalidRequest, "task id is required")
		}
	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "unsupported operation: "+env.Op)
	}
	return call, nil
}

// submitBody 把客户端请求体（JSON 对象，可为空）与查询参数 url 合并为 v4 提交请求体；必须有非空的 url（MU-02）。
func submitBody(body []byte, queryURL string) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
		if trimmed[0] != '{' {
			return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object with the document url (for a local file use POST /v1/parse/file or /v1/tasks/file)")
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "request body: "+err.Error())
		}
	}
	if _, ok := obj["url"]; !ok && queryURL != "" {
		raw, err := json.Marshal(queryURL)
		if err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "url: "+err.Error())
		}
		obj["url"] = raw
	}
	var u string
	if raw, ok := obj["url"]; !ok || json.Unmarshal(raw, &u) != nil || strings.TrimSpace(u) == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "url is required: a public URL of the document (for a local file use POST /v1/parse/file or /v1/tasks/file)")
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body: "+err.Error())
	}
	return out, nil
}

// uploadBody 由查询参数构造申请上传地址的请求体（MU-07）。
func uploadBody(params map[string]string) ([]byte, error) {
	name := params["name"]
	if !validFileName(name) {
		return nil, apperr.New(apperr.CodeInvalidRequest, "name is required: the file name with its extension, e.g. ?name=report.pdf")
	}
	if n, err := strconv.ParseInt(params["size"], 10, 64); err != nil || n <= 0 {
		return nil, apperr.New(apperr.CodeInvalidRequest, "Content-Length is required: send the file as the raw request body")
	}
	file := map[string]any{"name": name}
	top := map[string]any{"files": []any{file}}
	for _, k := range []string{"is_ocr", "enable_formula", "enable_table"} {
		v, ok := params[k]
		if !ok {
			continue
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, k+" must be true or false")
		}
		if k == "is_ocr" {
			file[k] = b
		} else {
			top[k] = b
		}
	}
	for _, k := range []string{"data_id", "page_ranges"} {
		if v, ok := params[k]; ok {
			file[k] = v
		}
	}
	for _, k := range []string{"language", "model_version"} {
		if v, ok := params[k]; ok {
			top[k] = v
		}
	}
	if v, ok := params["extra_formats"]; ok {
		var list []string
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				list = append(list, f)
			}
		}
		top["extra_formats"] = list
	}
	out, err := json.Marshal(top)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "upload options: "+err.Error())
	}
	return out, nil
}

// validFileName：1~255 字节，不含路径分隔符与控制字符，有非空扩展名（上游按扩展名识别格式）。
func validFileName(name string) bool {
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	dot := strings.LastIndexByte(name, '.')
	return dot > 0 && dot < len(name)-1
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
	case OpParseFile:
		return e.doParseFile(ctx, up, call)
	case OpTaskUpload:
		return e.doTaskUpload(ctx, up, call)
	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "mineru upstream does not support operation "+call.Op)
	}
}

// submit 提交任务。返回响应体与任务 ID。上游已接受（2xx）但读取失败时 accepted 为 true（可能已付费）。
func (e Egress) submit(ctx context.Context, up protocol.Upstream, call *protocol.Call) (body []byte, taskID string, accepted bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, protocol.JoinURL(up.BaseURL, pathTask), bytes.NewReader(call.Body))
	if err != nil {
		return nil, "", false, err
	}
	e.setHeaders(req, up.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, "", false, err
	}
	body, err = protocol.ReadBody(up, resp)
	if err != nil {
		return nil, "", true, err
	}
	if err := e.businessError(body); err != nil {
		return nil, "", false, err // 业务错误：任务没有创建，不计费
	}
	var r struct {
		Data struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Data.TaskID == "" {
		return nil, "", false, &protocol.AttemptError{
			Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError, Status: http.StatusBadGateway},
			Body:    body,
			Err:     errors.New("upstream did not return task id"),
		}
	}
	return body, r.Data.TaskID, true, nil
}

func (e Egress) doTaskCreate(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	body, taskID, accepted, err := e.submit(ctx, up, call)
	if err != nil {
		if accepted {
			return paidSubmission(call, 0), err // CORE-12：上游已接受，按估计计费
		}
		return nil, err
	}
	// spec/08 §8.2: 异步透传提交时按估计页数结算（UsageEstimated）
	est, _ := call.State.(pricing.Usage)
	usage := pricing.Usage{UnitRequests: 1}
	if p := est.Get(UnitPages); p > 0 {
		usage[UnitPages] = p
	}
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   body,
		Pins:   []string{taskID},
		Meter:  &meter{usage: usage, src: protocol.UsageEstimated},
	}, nil
}

// taskState 是 GET /api/v4/extract/task/{id} 中 data 的相关字段。
type taskState struct {
	State      string `json:"state"`
	FullZipURL string `json:"full_zip_url"`
	ErrMsg     string `json:"err_msg"`
	Progress   struct {
		TotalPages int64 `json:"total_pages"`
	} `json:"extract_progress"`
}

// getTask 查询任务。ID 以 batch- 开头时查询上传文件的批次结果（MU-08）。
// 业务错误（例如 -60012 找不到任务）返回 *AttemptError。
func (e Egress) getTask(ctx context.Context, up protocol.Upstream, id string) ([]byte, *taskState, error) {
	if batchID, ok := strings.CutPrefix(id, batchIDPrefix); ok {
		return e.getBatch(ctx, up, id, batchID)
	}
	body, err := e.getJSON(ctx, up, pathTask+"/"+url.PathEscape(id))
	if err != nil {
		return nil, nil, err
	}
	var r struct {
		Data taskState `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, nil, badUpstream(body, fmt.Errorf("decode task state: %w", err))
	}
	r.Data.State = strings.ToLower(strings.TrimSpace(r.Data.State))
	return body, &r.Data, nil
}

// getBatch 查询上传文件的解析结果，并转换为与单任务查询相同的形状（MU-08）：
// {"code":0,"msg":"ok","data":{"task_id":"batch-…","batch_id","file_name","state","full_zip_url","err_msg","extract_progress"}}。
// 刚上传时结果列表可能为空，视为 waiting-file。
func (e Egress) getBatch(ctx context.Context, up protocol.Upstream, id, batchID string) ([]byte, *taskState, error) {
	body, err := e.getJSON(ctx, up, pathBatch+url.PathEscape(batchID))
	if err != nil {
		return nil, nil, err
	}
	var r struct {
		Data struct {
			Results []struct {
				FileName   string          `json:"file_name"`
				State      string          `json:"state"`
				FullZipURL string          `json:"full_zip_url"`
				ErrMsg     string          `json:"err_msg"`
				DataID     string          `json:"data_id"`
				Progress   json.RawMessage `json:"extract_progress"`
			} `json:"extract_result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, nil, badUpstream(body, fmt.Errorf("decode batch state: %w", err))
	}
	data := map[string]any{"task_id": id, "batch_id": batchID, "state": "waiting-file"}
	st := &taskState{State: "waiting-file"}
	if len(r.Data.Results) > 0 {
		x := r.Data.Results[0]
		st.State = strings.ToLower(strings.TrimSpace(x.State))
		st.FullZipURL, st.ErrMsg = x.FullZipURL, x.ErrMsg
		data["state"], data["file_name"], data["err_msg"] = st.State, x.FileName, x.ErrMsg
		if x.FullZipURL != "" {
			data["full_zip_url"] = x.FullZipURL
		}
		if x.DataID != "" {
			data["data_id"] = x.DataID
		}
		if len(x.Progress) > 0 && string(x.Progress) != "null" {
			data["extract_progress"] = x.Progress
			if json.Unmarshal(x.Progress, &st.Progress) != nil {
				st.Progress.TotalPages = 0
			}
		}
	}
	out, err := json.Marshal(map[string]any{"code": 0, "msg": "ok", "data": data})
	if err != nil {
		return nil, nil, badUpstream(body, err)
	}
	return out, st, nil
}

// getJSON 以上游 Key 发送 GET 并检查业务码。
func (e Egress) getJSON(ctx context.Context, up protocol.Upstream, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, protocol.JoinURL(up.BaseURL, path), nil)
	if err != nil {
		return nil, err
	}
	e.setHeaders(req, up.Secret)
	resp, err := protocol.Send(up, req)
	if err != nil {
		return nil, err
	}
	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return nil, err
	}
	if err := e.businessError(body); err != nil {
		return nil, err
	}
	return body, nil
}

// badUpstream 是上游响应无法理解时的错误（502，可换 Key 重试）。
func badUpstream(body []byte, err error) error {
	return &protocol.AttemptError{
		Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError, Status: http.StatusBadGateway},
		Body:    body,
		Err:     err,
	}
}

func (e Egress) doTaskGet(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	body, _, err := e.getTask(ctx, up, call.Params["id"])
	if err != nil {
		return nil, err
	}
	// spec/08 §8.2: 查询不计费，Meter 返回 (nil, UsageUpstream)
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   body,
		Meter:  &meter{usage: nil, src: protocol.UsageUpstream},
	}, nil
}

// doTaskResult 查询任务，完成后原样转发结果压缩包（MU-05）。
func (e Egress) doTaskResult(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	_, st, err := e.getTask(ctx, up, call.Params["id"])
	if err != nil {
		return nil, err
	}
	if st.State != "done" || st.FullZipURL == "" {
		return nil, notFinished(st.State)
	}
	resp, err := e.fetchZip(ctx, up, st.FullZipURL)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	protocol.CopyHeaders(h, resp.Header, "Content-Type", "Content-Length")
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/zip")
	}
	h.Set("Content-Disposition", `attachment; filename="`+resultFileName(call.Params["id"])+`"`)
	// spec/08 §8.2: 下载不计费，ResponseRaw 流式转发
	return &protocol.Response{
		Kind:   protocol.ResponseRaw,
		Status: http.StatusOK,
		Header: h,
		Raw:    resp.Body,
		Meter:  &meter{usage: nil, src: protocol.UsageUpstream},
	}, nil
}

// notFinished 是任务尚未完成时下载结果的错误：409，不惩罚 Key。
func notFinished(state string) error {
	if state == "" {
		state = "unknown"
	}
	body, _ := json.Marshal(map[string]any{"code": http.StatusConflict, "msg": "task is not finished (state: " + state + ")", "state": state}) // map 的 Marshal 不会失败
	return &protocol.AttemptError{
		Outcome: protocol.Outcome{Kind: protocol.OutcomeClientError, Status: http.StatusConflict},
		Body:    body,
		Header:  http.Header{"Content-Type": {"application/json"}},
		Err:     errors.New("mineru task not finished: " + state),
	}
}

func resultFileName(id string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, id)
	if clean == "" {
		clean = "result"
	}
	return clean + ".zip"
}

func (e Egress) doParse(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	_, taskID, accepted, err := e.submit(ctx, up, call)
	if err != nil {
		if accepted {
			return paidSubmission(call, 0), err
		}
		return nil, err
	}
	return e.finishParse(ctx, up, call, taskID)
}

// doParseFile 上传本地文件后同步等待结果（MU-07）。
func (e Egress) doParseFile(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	batchID, accepted, err := e.upload(ctx, up, call)
	if err != nil {
		if accepted {
			return paidSubmission(call, 0), err
		}
		return nil, err
	}
	return e.finishParse(ctx, up, call, batchIDPrefix+batchID)
}

// doTaskUpload 上传本地文件后立即返回网关任务 ID batch-<batch_id>（MU-08），并固定到本 Key。
func (e Egress) doTaskUpload(ctx context.Context, up protocol.Upstream, call *protocol.Call) (*protocol.Response, error) {
	batchID, accepted, err := e.upload(ctx, up, call)
	if err != nil {
		if accepted {
			return paidSubmission(call, 0), err
		}
		return nil, err
	}
	id := batchIDPrefix + batchID
	body, err := json.Marshal(map[string]any{"code": 0, "msg": "ok", "data": map[string]string{"task_id": id, "batch_id": batchID}})
	if err != nil {
		return paidSubmission(call, 0), err
	}
	est, _ := call.State.(pricing.Usage)
	usage := pricing.Usage{UnitRequests: 1}
	if p := est.Get(UnitPages); p > 0 {
		usage[UnitPages] = p
	}
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   body,
		Pins:   []string{id},
		Meter:  &meter{usage: usage, src: protocol.UsageEstimated},
	}, nil
}

// upload 申请上传地址并把请求体上传过去（MU-07）。上传成功（accepted）后上游会自动提交解析任务。
func (e Egress) upload(ctx context.Context, up protocol.Upstream, call *protocol.Call) (batchID string, accepted bool, err error) {
	// 1. 申请上传地址（只有 JSON，不消耗客户端请求体）
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, protocol.JoinURL(up.BaseURL, pathUpload), bytes.NewReader(call.Body))
	if err != nil {
		return "", false, err
	}
	e.setHeaders(req, up.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := protocol.Send(up, req)
	if err != nil {
		return "", false, err
	}
	body, err := protocol.ReadBody(up, resp)
	if err != nil {
		return "", false, err
	}
	if err := e.businessError(body); err != nil {
		return "", false, err
	}
	var r struct {
		Data struct {
			BatchID  string   `json:"batch_id"`
			FileURLs []string `json:"file_urls"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Data.BatchID == "" || len(r.Data.FileURLs) == 0 {
		return "", false, badUpstream(body, errors.New("upstream did not return batch_id and file_urls"))
	}

	// 2. 把文件 PUT 到签名地址：定长、不设 Content-Type；第三方存储不带上游 Key
	target, sameOrigin, ok := allowedURL(r.Data.FileURLs[0], up.BaseURL)
	if !ok {
		return "", false, badUpstream(nil, errors.New("upstream returned an unsupported upload url"))
	}
	size, err := strconv.ParseInt(call.Params["size"], 10, 64)
	if err != nil || size <= 0 {
		return "", false, apperr.New(apperr.CodeInvalidRequest, "Content-Length is required")
	}
	src := call.BodyStream
	if src == nil {
		src = http.NoBody
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, target, src)
	if err != nil {
		return "", false, err
	}
	put.ContentLength = size
	put.Header.Set("User-Agent", e.userAgent())
	if sameOrigin {
		put.Header.Set("Authorization", "Bearer "+string(up.Secret))
	}
	presp, err := protocol.Send(up, put)
	if err != nil {
		return "", false, err // 文件没有完整上传，上游不会创建任务：不计费
	}
	if _, err := protocol.ReadBody(up, presp); err != nil {
		return r.Data.BatchID, true, afterSubmit(batchIDPrefix+r.Data.BatchID, err)
	}
	return r.Data.BatchID, true, nil
}

// allowedURL 检查上游返回的地址（MU-05、MU-07）：https，或与 base_url 同源。sameOrigin 时才可以带上游 Key。
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

// finishParse 轮询任务直到完成，下载结果包并返回 full.md（MU-03）。任务已提交：失败不重试且保留计量。
func (e Egress) finishParse(ctx context.Context, up protocol.Upstream, call *protocol.Call, taskID string) (result *protocol.Response, err error) {
	var pages int64
	// CORE-12：提交成功即可能计费；后续轮询、下载失败或取消也保留计量结果。
	defer func() {
		if err != nil {
			result = paidSubmission(call, pages)
		}
	}()

	// 2. 轮询直到完成（EG-02：1s 起翻倍到 10s，select 等待）
	interval, maxInterval := e.initialPollInterval(), e.maxPollInterval()
	var st *taskState
	for {
		select {
		case <-ctx.Done():
			return nil, afterSubmit(taskID, &protocol.AttemptError{Outcome: protocol.ClassifyNetError(ctx.Err(), ctx), Err: ctx.Err()})
		case <-time.After(interval):
		}
		var body []byte
		body, st, err = e.getTask(ctx, up, taskID)
		if err != nil {
			return nil, afterSubmit(taskID, err)
		}
		if st.Progress.TotalPages > 0 {
			pages = st.Progress.TotalPages
		}
		if st.State == "done" {
			break
		}
		if st.State == "failed" {
			msg := st.ErrMsg
			if msg == "" {
				msg = "mineru task failed"
			}
			return nil, &protocol.AttemptError{
				Outcome: protocol.Outcome{Kind: protocol.OutcomeClientError, Status: http.StatusBadRequest},
				Body:    body,
				Header:  http.Header{"Content-Type": {"application/json"}},
				Err:     errors.New(msg),
			}
		}
		interval = min(interval*2, maxInterval)
	}

	// 3. 下载结果包，取出 full.md
	if st.FullZipURL == "" {
		return nil, afterSubmit(taskID, &protocol.AttemptError{
			Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError, Status: http.StatusBadGateway},
			Err:     errors.New("task done without full_zip_url"),
		})
	}
	zresp, err := e.fetchZip(ctx, up, st.FullZipURL)
	if err != nil {
		return nil, afterSubmit(taskID, err)
	}
	zbody, err := protocol.ReadBody(up, zresp)
	if err != nil {
		return nil, afterSubmit(taskID, err)
	}
	md, zpages, err := readResultZip(zbody, maxBuffered(up))
	if err != nil {
		return nil, afterSubmit(taskID, &protocol.AttemptError{
			Outcome: protocol.Outcome{Kind: protocol.OutcomeServerError, Status: http.StatusBadGateway},
			Err:     err,
		})
	}
	if pages == 0 {
		pages = zpages
	}

	// spec/08 §8.2: 同步封装完成时按上游报告的实际页数结算（UsageUpstream）。
	// 页数未知时不能按 0 页结算（少计费），返回 UsageNone 让内核按预留用量结算（spec/06 §3.3）。
	m := &meter{src: protocol.UsageNone}
	if pages > 0 {
		m = &meter{usage: pricing.Usage{UnitRequests: 1, UnitPages: pages}, src: protocol.UsageUpstream}
	}
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"text/markdown; charset=utf-8"}},
		Body:   md,
		Meter:  m,
	}, nil
}

// fetchZip 下载结果压缩包（MU-05）：只允许 https 或与 base_url 同源的地址；
// 只有与 base_url 同源（即上游本身）时才带上游 Key，第三方 CDN 永远拿不到 Key。
func (e Egress) fetchZip(ctx context.Context, up protocol.Upstream, raw string) (*http.Response, error) {
	target, sameOrigin, ok := allowedURL(raw, up.BaseURL)
	if !ok {
		return nil, badUpstream(nil, errors.New("upstream returned an unsupported result url"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", e.userAgent())
	req.Header.Set("Accept", "*/*")
	if sameOrigin {
		req.Header.Set("Authorization", "Bearer "+string(up.Secret))
	}
	return protocol.Send(up, req)
}

// readResultZip 从结果包中取出 full.md；页数取 layout.json 的 pdf_info 长度（0 = 未知）。
// 解压后的内容同样受 limit 约束（防压缩炸弹）。
func readResultZip(b []byte, limit int64) (md []byte, pages int64, err error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, 0, fmt.Errorf("open result zip: %w", err)
	}
	var mdFile, layoutFile *zip.File
	for _, f := range zr.File {
		switch {
		case f.Name == "full.md" || strings.HasSuffix(f.Name, "/full.md"):
			if mdFile == nil {
				mdFile = f
			}
		case f.Name == "layout.json" || strings.HasSuffix(f.Name, "/layout.json"):
			if layoutFile == nil {
				layoutFile = f
			}
		}
	}
	if mdFile == nil {
		return nil, 0, errors.New("result zip has no full.md")
	}
	md, err = readZipFile(mdFile, limit)
	if err != nil {
		return nil, 0, err
	}
	if layoutFile != nil {
		if lb, lerr := readZipFile(layoutFile, limit); lerr == nil {
			var layout struct {
				PdfInfo []json.RawMessage `json:"pdf_info"`
			}
			if json.Unmarshal(lb, &layout) == nil {
				pages = int64(len(layout.PdfInfo))
			}
		}
	}
	return md, pages, nil
}

func readZipFile(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close() // 只读，关闭错误无意义
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", f.Name, limit)
	}
	return b, nil
}

func maxBuffered(up protocol.Upstream) int64 {
	if up.MaxBufferedBytes > 0 {
		return up.MaxBufferedBytes
	}
	return 64 << 20
}

// businessError 解析 v4 的业务码（HTTP 200 + {"code": ...}），非 0 时按 MU-04 分类为 *AttemptError。
// 响应不是 JSON 或没有 code 字段时返回 nil，由调用方按内容判断。
func (e Egress) businessError(body []byte) error {
	var r struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
	}
	if json.Unmarshal(body, &r) != nil || len(r.Code) == 0 {
		return nil
	}
	code := strings.Trim(string(r.Code), `"`)
	if code == "0" || code == "null" {
		return nil
	}
	return &protocol.AttemptError{
		Outcome: businessOutcome(code, e.now()),
		Body:    body,
		Header:  http.Header{"Content-Type": {"application/json"}},
		Err:     fmt.Errorf("mineru error %s: %s", code, r.Msg),
	}
}

// businessOutcome 是 MU-04 的业务码分类。
func businessOutcome(code string, now time.Time) protocol.Outcome {
	switch code {
	case "A0202", "A0211": // Token 错误 / 过期
		return protocol.Outcome{Kind: protocol.OutcomeAuthFailed, Status: http.StatusUnauthorized}
	case "-60018", "-60019": // 每日额度用完：冷却到北京时间次日 0 点（受池的 cooldown_max 约束）
		return protocol.Outcome{Kind: protocol.OutcomeRateLimited, Status: http.StatusTooManyRequests, RetryAfter: untilQuotaReset(now)}
	case "-60009", "-60007", "-10001": // 队列已满 / 模型服务不可用 / 服务异常
		return protocol.Outcome{Kind: protocol.OutcomeOverloaded, Status: http.StatusServiceUnavailable}
	case "-60012", "-60013": // 找不到任务 / 无权访问该任务
		return protocol.Outcome{Kind: protocol.OutcomeClientError, Status: http.StatusNotFound}
	default:
		return protocol.Outcome{Kind: protocol.OutcomeClientError, Status: http.StatusBadRequest}
	}
}

// quotaZone 是 MinerU 日额度的重置时区（北京时间，固定 UTC+8，无夏令时）。
var quotaZone = time.FixedZone("UTC+8", 8*3600)

func untilQuotaReset(now time.Time) time.Duration {
	t := now.In(quotaZone)
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 1, 0, 0, quotaZone)
	return next.Sub(now)
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

func (e Egress) setHeaders(req *http.Request, secret []byte) {
	req.Header.Set("Authorization", "Bearer "+string(secret))
	req.Header.Set("User-Agent", e.userAgent())
	req.Header.Set("Accept", "application/json")
}

func (e Egress) userAgent() string {
	if e.UserAgent != "" {
		return e.UserAgent
	}
	return "universal-ai-gateway"
}

func (e Egress) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
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
