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
}

// APIUsage 是一个计量单位的取值规则（DA-07）。
type APIUsage struct {
	JSON         string           `yaml:"json"`
	RequestJSON  string           `yaml:"request_json"`
	RequestQuery string           `yaml:"request_query"`
	Map          map[string]int64 `yaml:"map"`
	Default      *int64           `yaml:"default"`
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
		_, segs, err := ParseAPIMatch(e.Match)
		if err != nil {
			return fieldErr(ep+".match", "%v", err)
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
			if u.JSON == "" && u.RequestJSON == "" && u.RequestQuery == "" && u.Default == nil {
				return fieldErr(up, "needs json, request_json, request_query or default")
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
