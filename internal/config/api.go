package config

// 服务的声明式 API 能力（spec/03 CFG-20，spec/08 §8.3.6，任务卡 T66）。
// 配置包只做与协议无关的静态校验；「只对 rest 池的服务有效」由装配代码检查（DA-01）。

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
)

// APIConfig 是 models[].api。
type APIConfig struct {
	// Errors 按顺序匹配，第一条命中的生效（DA-03 ~ DA-05）。
	Errors []APIErrorRule `yaml:"errors"`
	// Endpoints 按顺序匹配「方法 + 路径模板」，第一条命中的生效（DA-06 ~ DA-10）。
	Endpoints []APIEndpoint `yaml:"endpoints"`
}

// APIErrorRule 是一条业务错误映射。
type APIErrorRule struct {
	When          APIWhen      `yaml:"when"`
	Outcome       string       `yaml:"outcome"`        // 见 APIOutcomes
	Status        int          `yaml:"status"`         // 只用于 client_error：返回给客户端的状态码（缺省 400）
	CooldownUntil *APICooldown `yaml:"cooldown_until"` // 只用于 rate_limited
}

// APIWhen 是错误规则的条件；全部满足才算命中。
type APIWhen struct {
	Status []int  `yaml:"status"`
	JSON   string `yaml:"json"`   // 字段路径（DA-02）
	In     []any  `yaml:"in"`     // 字符串、整数、小数或布尔值
	NotIn  []any  `yaml:"not_in"` // 同上
	Exists *bool  `yaml:"exists"`
}

// APICooldown 是「冷却到下一个某时刻」（DA-05）。
type APICooldown struct {
	Time       string `yaml:"time"`         // HH:MM
	DayOfMonth int    `yaml:"day_of_month"` // 0 = 每天；1 ~ 28 = 每月这一天
	TZ         string `yaml:"tz"`           // ±HH:MM 或 UTC；缺省 UTC
}

// APIEndpoint 是一个端点的声明。
type APIEndpoint struct {
	Match    string `yaml:"match"`     // "METHOD /path/{placeholder}"
	TaskID   string `yaml:"task_id"`   // 成功响应中任务 ID 的字段路径（DA-08）
	PinnedBy string `yaml:"pinned_by"` // 路径占位符名（DA-09）
	// Usage 为 nil 表示未声明（按次计费）；非 nil 的空映射表示不计费（DA-07）。
	Usage map[string]APIUsage `yaml:"usage"`
	// Flow 非 nil 时 match 是网关上的虚拟路径，执行多步上游请求（FL-01，T67）。
	Flow *APIFlow `yaml:"flow"`
}

// APIFlow 是虚拟端点的多步请求（FL-01 ~ FL-11）。Submit 与 Upload 恰好一个。
type APIFlow struct {
	Submit *APIFlowSubmit `yaml:"submit"`
	Upload *APIFlowUpload `yaml:"upload"`
	Poll   *APIFlowPoll   `yaml:"poll"`
	Result *APIFlowResult `yaml:"result"`
}

// APIFlowSubmit 转发客户端请求体（FL-02）。
type APIFlowSubmit struct {
	Request string `yaml:"request"` // "METHOD /upstream/path"
	TaskID  string `yaml:"task_id"` // 字段路径
}

// APIFlowUpload 申请上传地址并把客户端请求体 PUT 过去（FL-03、FL-04）。
type APIFlowUpload struct {
	Request   string                  `yaml:"request"`
	Body      map[string]APIBodyField `yaml:"body"` // 目标字段路径 → 来源
	UploadURL string                  `yaml:"upload_url"`
	TaskID    string                  `yaml:"task_id"`
}

// APIBodyField 是申请上传地址请求体中一个字段的来源（FL-04）。
type APIBodyField struct {
	Query    string `yaml:"query"`
	Type     string `yaml:"type"` // string（缺省）、bool、int、list
	Required bool   `yaml:"required"`
}

// APIFlowPoll 是轮询（FL-05）。
type APIFlowPoll struct {
	Request string        `yaml:"request"` // 路径必须含 {task_id}
	State   string        `yaml:"state"`
	Done    []string      `yaml:"done"`
	Failed  []string      `yaml:"failed"`
	Timeout time.Duration `yaml:"timeout"` // 缺省 10 分钟，上限 1 小时
}

// APIFlowResult 是同步结果（FL-06）。
type APIFlowResult struct {
	URL         string `yaml:"url"`
	ZipEntry    string `yaml:"zip_entry"`
	ContentType string `yaml:"content_type"`
}

// APIBodyTypes 是 FL-04 的字段类型。
var APIBodyTypes = map[string]bool{"": true, "string": true, "bool": true, "int": true, "list": true}

// MaxFlowPollTimeout 是 poll.timeout 的上限（FL-05）。
const MaxFlowPollTimeout = time.Hour

// validateFlow 实现 CFG-20 对 flow 的静态校验。
func validateFlow(ep string, method string, e APIEndpoint) error {
	f := e.Flow
	if (f.Submit == nil) == (f.Upload == nil) {
		return fieldErr(ep+".flow", "needs exactly one of submit or upload")
	}
	if e.TaskID != "" || e.PinnedBy != "" {
		return fieldErr(ep, "task_id and pinned_by cannot be used with flow (put task_id inside the flow step)")
	}
	stepRequest := func(fp, req string) error {
		_, segs, err := ParseAPIMatch(req)
		if err != nil {
			return fieldErr(fp+".request", "%v", err)
		}
		for _, s := range segs {
			if strings.HasPrefix(s, "{") {
				return fieldErr(fp+".request", "placeholders are only allowed in poll.request")
			}
		}
		return nil
	}
	taskID := ""
	if s := f.Submit; s != nil {
		if err := stepRequest(ep+".flow.submit", s.Request); err != nil {
			return err
		}
		taskID = s.TaskID
	}
	if u := f.Upload; u != nil {
		up := ep + ".flow.upload"
		if method != "POST" && method != "PATCH" {
			return fieldErr(ep+".match", "an upload flow needs POST or PATCH (the file is the request body)")
		}
		if err := stepRequest(up, u.Request); err != nil {
			return err
		}
		if _, err := ParseFieldPath(u.UploadURL); err != nil {
			return fieldErr(up+".upload_url", "%v", err)
		}
		for target, src := range u.Body {
			if _, err := ParseFieldPath(target); err != nil {
				return fieldErr(up+".body", "%v", err)
			}
			if src.Query == "" {
				return fieldErr(up+".body."+target+".query", "must not be empty")
			}
			if !APIBodyTypes[src.Type] {
				return fieldErr(up+".body."+target+".type", "must be string, bool, int or list, got %q", src.Type)
			}
		}
		taskID = u.TaskID
	}
	if taskID != "" {
		if _, err := ParseFieldPath(taskID); err != nil {
			return fieldErr(ep+".flow", "task_id: %v", err)
		}
	}
	if p := f.Poll; p != nil {
		pp := ep + ".flow.poll"
		if taskID == "" {
			return fieldErr(ep+".flow", "poll needs task_id in submit or upload")
		}
		_, segs, err := ParseAPIMatch(p.Request)
		if err != nil {
			return fieldErr(pp+".request", "%v", err)
		}
		hasTask := false
		for _, s := range segs {
			if strings.HasPrefix(s, "{") {
				if s != "{task_id}" {
					return fieldErr(pp+".request", "only the {task_id} placeholder is allowed, got %s", s)
				}
				hasTask = true
			}
		}
		if !hasTask {
			return fieldErr(pp+".request", "must contain {task_id}")
		}
		if _, err := ParseFieldPath(p.State); err != nil {
			return fieldErr(pp+".state", "%v", err)
		}
		if len(p.Done) == 0 {
			return fieldErr(pp+".done", "must not be empty")
		}
		if p.Timeout < 0 || p.Timeout > MaxFlowPollTimeout {
			return fieldErr(pp+".timeout", "must be between 0 (default 10m) and 1h")
		}
	}
	if r := f.Result; r != nil {
		rp := ep + ".flow.result"
		if f.Poll == nil {
			return fieldErr(rp, "needs poll")
		}
		if r.URL != "" {
			if _, err := ParseFieldPath(r.URL); err != nil {
				return fieldErr(rp+".url", "%v", err)
			}
		}
		if r.ZipEntry != "" && (r.URL == "" || strings.ContainsAny(r.ZipEntry, "/\\") || r.ZipEntry == "." || r.ZipEntry == "..") {
			return fieldErr(rp+".zip_entry", "needs url and must be a plain file name")
		}
	}
	return nil
}

// APIUsage 是一个计量单位的取值规则（DA-07）。
type APIUsage struct {
	JSON         string           `yaml:"json"`
	RequestJSON  string           `yaml:"request_json"`
	RequestQuery string           `yaml:"request_query"`
	Map          map[string]int64 `yaml:"map"`
	Default      *int64           `yaml:"default"`
	// ZipJSON 从 flow 下载的结果包中读取（FL-09a，T67）：只用于 result 有 zip_entry 的 flow。
	ZipJSON *APIZipJSON `yaml:"zip_json"`
}

// APIZipJSON 是「结果包中某个 JSON 文件的某个字段」：数组取长度，数字取原值（FL-09a）。
type APIZipJSON struct {
	Entry string `yaml:"entry"` // 文件名（不含目录）
	Count string `yaml:"count"` // 字段路径
}

// APIOutcomes 是 outcome 的合法取值（DA-04）。
var APIOutcomes = map[string]bool{
	"rate_limited": true, "auth_failed": true, "permission_denied": true,
	"overloaded": true, "server_error": true, "client_error": true,
}

var apiMethods = map[string]bool{"GET": true, "DELETE": true, "POST": true, "PUT": true, "PATCH": true}

// ParseAPIMatch 解析 "METHOD /path"：返回大写方法与路径段；占位符必须独占一段（DA-10）。
func ParseAPIMatch(s string) (method string, segments []string, err error) {
	m, path, ok := strings.Cut(strings.TrimSpace(s), " ")
	path = strings.TrimSpace(path)
	m = strings.ToUpper(m)
	if !ok || !apiMethods[m] {
		return "", nil, fmt.Errorf(`must look like "POST /path", with method GET, DELETE, POST, PUT or PATCH`)
	}
	if !strings.HasPrefix(path, "/") {
		return "", nil, fmt.Errorf("path must start with /")
	}
	if strings.ContainsAny(path, "?#\\ \t") {
		return "", nil, fmt.Errorf(`path must not contain ?, #, \ or whitespace`)
	}
	seen := map[string]bool{}
	segs := strings.Split(path[1:], "/")
	if path == "/" {
		segs = nil
	}
	for _, seg := range segs {
		if strings.ContainsAny(seg, "{}") {
			if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' || !toolPlaceholder.MatchString(seg[1:len(seg)-1]) {
				return "", nil, fmt.Errorf("invalid placeholder segment %q (a placeholder must be a whole segment like {id})", seg)
			}
			if seen[seg] {
				return "", nil, fmt.Errorf("placeholder %s appears more than once", seg)
			}
			seen[seg] = true
			continue
		}
		dec, err := url.PathUnescape(seg)
		if err != nil || dec == "." || dec == ".." {
			return "", nil, fmt.Errorf("invalid path segment %q", seg)
		}
	}
	return m, segs, nil
}

// ParseFieldPath 拆分字段路径：点分隔，每段非空（DA-02）。纯数字段表示数组下标。
func ParseFieldPath(p string) ([]string, error) {
	if p == "" {
		return nil, fmt.Errorf("must not be empty")
	}
	segs := strings.Split(p, ".")
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("invalid field path %q (empty segment)", p)
		}
	}
	return segs, nil
}

// ParseTZ 解析 ±HH:MM 或 UTC（空串 = UTC）。
func ParseTZ(s string) (*time.Location, error) {
	switch s {
	case "", "UTC", "Z":
		return time.UTC, nil
	}
	if len(s) == 6 && (s[0] == '+' || s[0] == '-') && s[3] == ':' {
		h, err1 := strconv.Atoi(s[1:3])
		m, err2 := strconv.Atoi(s[4:6])
		if err1 == nil && err2 == nil && h <= 14 && m < 60 {
			off := h*3600 + m*60
			if s[0] == '-' {
				off = -off
			}
			return time.FixedZone("UTC"+s, off), nil
		}
	}
	return nil, fmt.Errorf("time zone must be ±HH:MM or UTC, got %q", s)
}

// ParseClock 解析 HH:MM。
func ParseClock(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, 0, fmt.Errorf("time must be HH:MM, got %q", s)
	}
	return t.Hour(), t.Minute(), nil
}

func validScalar(v any) bool {
	switch v.(type) {
	case string, int, int64, uint64, float64, bool:
		return true
	}
	return false
}

// validateAPI 实现 CFG-20：models[].api 的静态校验。
func validateAPI(path string, m ModelConfig) error {
	a := m.API
	for i, r := range a.Errors {
		rp := fmt.Sprintf("%s.api.errors[%d]", path, i)
		w := r.When
		if len(w.Status) == 0 && w.JSON == "" {
			return fieldErr(rp+".when", "needs status or json")
		}
		for _, s := range w.Status {
			if s < 100 || s > 599 {
				return fieldErr(rp+".when.status", "must be HTTP status codes, got %d", s)
			}
		}
		if w.JSON != "" {
			if _, err := ParseFieldPath(w.JSON); err != nil {
				return fieldErr(rp+".when.json", "%v", err)
			}
			if len(w.In) == 0 && len(w.NotIn) == 0 && w.Exists == nil {
				return fieldErr(rp+".when", "json needs in, not_in or exists")
			}
		} else if len(w.In) > 0 || len(w.NotIn) > 0 || w.Exists != nil {
			return fieldErr(rp+".when", "in, not_in and exists need json")
		}
		for _, v := range append(append([]any{}, w.In...), w.NotIn...) {
			if !validScalar(v) {
				return fieldErr(rp+".when", "in / not_in values must be strings, numbers or booleans")
			}
		}
		if !APIOutcomes[r.Outcome] {
			return fieldErr(rp+".outcome", "must be rate_limited, auth_failed, permission_denied, overloaded, server_error or client_error, got %q", r.Outcome)
		}
		if r.Status != 0 && (r.Outcome != "client_error" || r.Status < 400 || r.Status > 499) {
			return fieldErr(rp+".status", "only allowed for client_error, and must be 400 ~ 499")
		}
		if c := r.CooldownUntil; c != nil {
			if r.Outcome != "rate_limited" {
				return fieldErr(rp+".cooldown_until", "only allowed for rate_limited")
			}
			if _, _, err := ParseClock(c.Time); err != nil {
				return fieldErr(rp+".cooldown_until.time", "%v", err)
			}
			if c.DayOfMonth < 0 || c.DayOfMonth > 28 {
				return fieldErr(rp+".cooldown_until.day_of_month", "must be 1 ~ 28 (or omitted for every day)")
			}
			if _, err := ParseTZ(c.TZ); err != nil {
				return fieldErr(rp+".cooldown_until.tz", "%v", err)
			}
		}
	}
	for i, e := range a.Endpoints {
		ep := fmt.Sprintf("%s.api.endpoints[%d]", path, i)
		method, segs, err := ParseAPIMatch(e.Match)
		if err != nil {
			return fieldErr(ep+".match", "%v", err)
		}
		if e.Flow != nil {
			if err := validateFlow(ep, method, e); err != nil {
				return err
			}
		}
		if e.TaskID != "" {
			if _, err := ParseFieldPath(e.TaskID); err != nil {
				return fieldErr(ep+".task_id", "%v", err)
			}
		}
		if e.PinnedBy != "" {
			found := false
			for _, s := range segs {
				found = found || s == "{"+e.PinnedBy+"}"
			}
			if !found {
				return fieldErr(ep+".pinned_by", "placeholder {%s} is not in match", e.PinnedBy)
			}
			if e.TaskID != "" {
				return fieldErr(ep, "task_id and pinned_by cannot be used together")
			}
		}
		for unit, u := range e.Usage {
			up := ep + ".usage." + unit
			if !pricing.ValidUnit(unit) {
				return fieldErr(ep+".usage", "invalid unit name %q", unit)
			}
			if _, ok := m.Price[unit]; !ok {
				return fieldErr(up, "unit %q must be priced in price (use \"0\" if free)", unit)
			}
			if u.RequestJSON != "" && u.RequestQuery != "" {
				return fieldErr(up, "use request_json or request_query, not both")
			}
			for _, fp := range []string{u.JSON, u.RequestJSON} {
				if fp != "" {
					if _, err := ParseFieldPath(fp); err != nil {
						return fieldErr(up, "%v", err)
					}
				}
			}
			if u.JSON == "" && u.RequestJSON == "" && u.RequestQuery == "" && u.Default == nil && u.ZipJSON == nil {
				return fieldErr(up, "needs json, request_json, request_query, zip_json or default")
			}
			if z := u.ZipJSON; z != nil {
				if e.Flow == nil || e.Flow.Result == nil || e.Flow.Result.ZipEntry == "" {
					return fieldErr(up+".zip_json", "only applies to a flow whose result has zip_entry")
				}
				if z.Entry == "" || strings.ContainsAny(z.Entry, "/\\") || z.Entry == "." || z.Entry == ".." {
					return fieldErr(up+".zip_json.entry", "must be a plain file name")
				}
				if _, err := ParseFieldPath(z.Count); err != nil {
					return fieldErr(up+".zip_json.count", "%v", err)
				}
			}
			if len(u.Map) > 0 && u.RequestJSON == "" && u.RequestQuery == "" {
				return fieldErr(up+".map", "only applies to request_json or request_query")
			}
			for k, v := range u.Map {
				if v < 0 {
					return fieldErr(up+".map."+k, "must be >= 0")
				}
			}
			if u.Default != nil && *u.Default < 0 {
				return fieldErr(up+".default", "must be >= 0")
			}
		}
	}
	return nil
}
