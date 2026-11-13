package rest

// 声明式 API 能力（spec/08 §8.3.6，任务卡 T66）：业务错误映射、用量提取、异步任务固定。
// 声明由装配代码从配置（models[].api）转换而来，构造后只读。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// APIDef 是一个服务的声明（DA-01 ~ DA-14）。
type APIDef struct {
	Service   string
	Errors    []ErrorRule
	Endpoints []EndpointDef
}

// ErrorRule 是一条业务错误映射（DA-03 ~ DA-05）。全部条件满足才命中。
type ErrorRule struct {
	Status   []int    // 状态码之一；空 = 不检查
	JSON     []string // 字段路径；nil = 不检查 JSON
	In       []any    // 字段值之一（string、整数、float64、bool）
	NotIn    []any    // 字段值不是其中任何一个（字段不存在时不命中）
	Exists   *bool    // 字段是否存在
	Outcome  protocol.OutcomeKind
	Client   int       // client_error 时返回给客户端的状态码（0 = 400）
	Cooldown *Cooldown // 只用于 rate_limited
}

// Cooldown 是「冷却到下一个某时刻」（DA-05）。DayOfMonth 为 0 表示每天。
type Cooldown struct {
	DayOfMonth   int
	Hour, Minute int
	Loc          *time.Location
}

// EndpointDef 是一个端点的声明（DA-06 ~ DA-10）。
type EndpointDef struct {
	Method   string   // 大写；GET 同时匹配 HEAD
	Segments []string // 路径段；"{name}" 为占位符
	TaskID   []string // 成功响应中任务 ID 的字段路径；nil = 不记录
	PinnedBy string   // 占位符名；空 = 不固定
	// Usage 为 nil 表示未声明（{requests: 1}）；非 nil 的空映射表示不计费。
	Usage map[string]UsageRule
	// Flow 非 nil 时这是虚拟端点（FL-01，T67）。
	Flow *FlowDef
}

// UsageRule 是一个计量单位的取值规则（DA-07）。
type UsageRule struct {
	JSON         []string
	RequestJSON  []string
	RequestQuery string
	Map          map[string]int64
	Default      *int64
	ZipJSON      *ZipCount // 只在 flow 的 zip 结果上求值（FL-09a）
}

// ZipCount 是结果包中某个 JSON 文件的某个字段：数组取长度，非负整数取原值（FL-09a）。
type ZipCount struct {
	Entry string
	Count []string
}

// apiTable 按服务名索引声明。
type apiTable struct {
	byService map[string]*APIDef
	units     []string
}

func newAPITable(defs []APIDef) (*apiTable, error) {
	t := &apiTable{byService: make(map[string]*APIDef, len(defs))}
	units := map[string]bool{UnitRequests: true}
	for i := range defs {
		d := &defs[i]
		if d.Service == "" {
			return nil, fmt.Errorf("rest: api declaration without service")
		}
		if _, dup := t.byService[d.Service]; dup {
			return nil, fmt.Errorf("rest: duplicate api declaration for service %q", d.Service)
		}
		for _, r := range d.Errors {
			if r.Cooldown != nil && r.Cooldown.Loc == nil {
				return nil, fmt.Errorf("rest: service %q: cooldown without time zone", d.Service)
			}
		}
		for _, e := range d.Endpoints {
			for u := range e.Usage {
				units[u] = true
			}
			if e.Flow != nil {
				if err := validateFlowDef(d.Service, e.Flow); err != nil {
					return nil, err
				}
			}
		}
		t.byService[d.Service] = d
	}
	for u := range units {
		t.units = append(t.units, u)
	}
	sort.Strings(t.units)
	return t, nil
}

// Units 返回 rest 可能报告的全部计量单位：所有声明中出现的单位加 requests（DA-13）。
func (t *apiTable) Units() []string {
	if t == nil {
		return []string{UnitRequests}
	}
	return append([]string(nil), t.units...)
}

func (t *apiTable) lookup(service string) *APIDef {
	if t == nil {
		return nil
	}
	return t.byService[service]
}

// match 返回命中的端点与占位符取值（DA-06、DA-10）；没有命中返回 nil。
// subpath 是转义形式（RS-04），按段解码后比较。
func (d *APIDef) match(method, subpath string) (*EndpointDef, map[string]string) {
	if d == nil {
		return nil, nil
	}
	if method == http.MethodHead {
		method = http.MethodGet
	}
	var segs []string
	if p := strings.TrimPrefix(subpath, "/"); p != "" {
		segs = strings.Split(p, "/")
	}
	for i := range d.Endpoints {
		e := &d.Endpoints[i]
		if e.Method != method || len(e.Segments) != len(segs) {
			continue
		}
		vars := map[string]string{}
		ok := true
		for j, want := range e.Segments {
			got, err := url.PathUnescape(segs[j])
			if err != nil {
				ok = false
				break
			}
			if strings.HasPrefix(want, "{") {
				if got == "" {
					ok = false
					break
				}
				vars[want[1:len(want)-1]] = got
			} else if got != want {
				ok = false
				break
			}
		}
		if ok {
			return e, vars
		}
	}
	return nil, nil
}

// pinValue 是固定记录的值：<服务名>:<任务 ID>（DA-08）。
func pinValue(service, id string) string { return service + ":" + id }

// decodeJSON 解析 JSON 文档；失败返回 ok=false。数字保留为 json.Number。
func decodeJSON(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// lookupPath 按字段路径取值（DA-02）；不存在或类型不符返回 ok=false。
func lookupPath(doc any, path []string) (any, bool) {
	cur := doc
	for _, seg := range path {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// equalJSON 按 JSON 原值比较（DA-03）：字符串只等于字符串，数字只等于数值相同的数字。
func equalJSON(got, want any) bool {
	switch w := want.(type) {
	case string:
		s, ok := got.(string)
		return ok && s == w
	case bool:
		b, ok := got.(bool)
		return ok && b == w
	}
	n, ok := got.(json.Number)
	if !ok {
		return false
	}
	g, err := n.Float64()
	if err != nil {
		return false
	}
	switch w := want.(type) {
	case int:
		return g == float64(w)
	case int64:
		return g == float64(w)
	case uint64:
		return g == float64(w)
	case float64:
		return g == w
	}
	return false
}

// matchError 返回第一条命中的错误规则（DA-03）。doc 为 nil 表示响应体不是 JSON。
func (d *APIDef) matchError(status int, doc any, isJSON bool) *ErrorRule {
	if d == nil {
		return nil
	}
	for i := range d.Errors {
		r := &d.Errors[i]
		if len(r.Status) > 0 && !containsInt(r.Status, status) {
			continue
		}
		if r.JSON != nil {
			if !isJSON {
				continue
			}
			v, present := lookupPath(doc, r.JSON)
			if r.Exists != nil && present != *r.Exists {
				continue
			}
			if len(r.In) > 0 && (!present || !anyEqual(v, r.In)) {
				continue
			}
			if len(r.NotIn) > 0 && (!present || anyEqual(v, r.NotIn)) {
				continue
			}
		}
		return r
	}
	return nil
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func anyEqual(v any, set []any) bool {
	for _, w := range set {
		if equalJSON(v, w) {
			return true
		}
	}
	return false
}

// hasJSONErrorRules 报告是否有需要读响应体的错误规则。
func (d *APIDef) hasJSONErrorRules() bool {
	if d == nil {
		return false
	}
	for _, r := range d.Errors {
		if r.JSON != nil {
			return true
		}
	}
	return false
}

// next 返回 now 之后（不含）第一个满足条件的时刻（DA-05）。
func (c *Cooldown) next(now time.Time) time.Time {
	local := now.In(c.Loc)
	if c.DayOfMonth == 0 {
		t := time.Date(local.Year(), local.Month(), local.Day(), c.Hour, c.Minute, 0, 0, c.Loc)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t
	}
	t := time.Date(local.Year(), local.Month(), c.DayOfMonth, c.Hour, c.Minute, 0, 0, c.Loc)
	if !t.After(now) {
		t = time.Date(local.Year(), local.Month()+1, c.DayOfMonth, c.Hour, c.Minute, 0, 0, c.Loc)
	}
	return t
}

// apply 把命中的规则写进 Outcome（DA-04、DA-05）。
func (r *ErrorRule) apply(o *protocol.Outcome, now time.Time) {
	o.Kind = r.Outcome
	if r.Outcome == protocol.OutcomePermissionDenied { // RS-16：403 不隔离 Key，按客户端错误处理
		o.Kind, o.Status = protocol.OutcomeClientError, http.StatusForbidden
	} else if r.Outcome == protocol.OutcomeClientError {
		o.Status = r.Client
		if o.Status == 0 {
			o.Status = http.StatusBadRequest
		}
	} else if o.Status < 400 {
		o.Status = defaultStatus(r.Outcome)
	}
	if r.Cooldown != nil {
		if d := r.Cooldown.next(now).Sub(now); d > 0 {
			o.RetryAfter = d
		}
	}
}

func defaultStatus(k protocol.OutcomeKind) int {
	switch k {
	case protocol.OutcomeRateLimited:
		return http.StatusTooManyRequests
	case protocol.OutcomeAuthFailed:
		return http.StatusUnauthorized
	case protocol.OutcomePermissionDenied:
		return http.StatusForbidden
	case protocol.OutcomeOverloaded:
		return http.StatusServiceUnavailable
	case protocol.OutcomeServerError:
		return http.StatusBadGateway
	}
	return http.StatusBadRequest
}

// needsBody 报告 2xx 响应是否需要缓冲读取（DA-11）。
func (e *EndpointDef) needsBody(d *APIDef) bool {
	if d.hasJSONErrorRules() {
		return true
	}
	if e == nil {
		return false
	}
	if e.TaskID != nil {
		return true
	}
	for _, u := range e.Usage {
		if u.JSON != nil {
			return true
		}
	}
	return false
}

// usageResult 是 DA-07 的计算结果。warn 列出取值不合法的单位（不含值）。
type usageResult struct {
	usage pricing.Usage
	src   protocol.UsageSource
	warn  []string
}

// computeUsage 实现 DA-06、DA-07。doc 是响应 JSON（可为 nil），reqDoc 是请求体 JSON（可为 nil）。
func computeUsage(e *EndpointDef, doc, reqDoc any, query url.Values) usageResult {
	if e == nil || e.Usage == nil {
		return usageResult{usage: pricing.Usage{UnitRequests: 1}, src: protocol.UsageUpstream}
	}
	if len(e.Usage) == 0 {
		return usageResult{src: protocol.UsageUpstream} // 确定不计费（EG-08）
	}
	res := usageResult{usage: pricing.Usage{}, src: protocol.UsageUpstream}
	estimated := false
	units := make([]string, 0, len(e.Usage))
	for u := range e.Usage {
		units = append(units, u)
	}
	sort.Strings(units)
	for _, unit := range units {
		r := e.Usage[unit]
		if r.JSON != nil && doc != nil {
			if v, ok := lookupPath(doc, r.JSON); ok {
				if n, ok := nonNegInt(v); ok {
					res.usage[unit] = n
					continue
				}
				res.warn = append(res.warn, unit)
			}
		}
		if n, ok, bad := requestValue(r, reqDoc, query); ok {
			res.usage[unit] = n
			estimated = true
			continue
		} else if bad {
			res.warn = append(res.warn, unit)
		}
		if r.Default != nil {
			res.usage[unit] = *r.Default
			estimated = true
		}
	}
	switch {
	case len(res.usage) == 0:
		res.usage, res.src = nil, protocol.UsageNone
	case estimated:
		res.src = protocol.UsageEstimated
	}
	return res
}

// requestValue 从请求体字段或查询参数取值，经 map 换算（DA-07）。bad 表示有值但不合法。
func requestValue(r UsageRule, reqDoc any, query url.Values) (n int64, ok, bad bool) {
	var raw any
	switch {
	case r.RequestJSON != nil:
		if reqDoc == nil {
			return 0, false, false
		}
		v, present := lookupPath(reqDoc, r.RequestJSON)
		if !present {
			return 0, false, false
		}
		raw = v
	case r.RequestQuery != "":
		if !query.Has(r.RequestQuery) {
			return 0, false, false
		}
		raw = json.Number(query.Get(r.RequestQuery))
	default:
		return 0, false, false
	}
	if len(r.Map) > 0 {
		key := valueText(raw)
		if m, hit := r.Map[key]; hit {
			return m, true, false
		}
		return 0, false, false // 不在换算表里：取缺省值
	}
	if n, ok := nonNegInt(raw); ok {
		return n, true, false
	}
	return 0, false, true
}

// nonNegInt 接受非负整数（json.Number 或纯数字字符串）。
func nonNegInt(v any) (int64, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = string(x)
	case string:
		s = x
	default:
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// valueText 返回标量的文本形式，用于 map 查找与任务 ID。
func valueText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

// taskIDFrom 取任务 ID（DA-08）：必须是非空字符串或数字，且不超过 256 字节。
func taskIDFrom(doc any, path []string) (string, bool) {
	v, ok := lookupPath(doc, path)
	if !ok {
		return "", false
	}
	switch v.(type) {
	case string, json.Number:
	default:
		return "", false
	}
	id := strings.TrimSpace(valueText(v))
	if id == "" || len(id) > 256 {
		return "", false
	}
	return id, true
}

// isJSONType 报告 Content-Type 是否为 JSON（application/json 或 *+json）；空也视为可能是 JSON。
func isJSONType(ct string) bool {
	if ct == "" {
		return true
	}
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}
