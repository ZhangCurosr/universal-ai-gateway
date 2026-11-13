package main

// 声明式 API 能力的装配（spec/08 §8.3.6，任务卡 T66）：把配置中的 models[].api 转换为 rest.APIDef，
// 检查 DA-01（只有 rest 池的服务可以声明），并为 rest 服务补齐未声明单位的价格（DA-13）。

import (
	"fmt"
	"maps"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/rest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

var apiOutcomeKinds = map[string]protocol.OutcomeKind{
	"rate_limited":      protocol.OutcomeRateLimited,
	"auth_failed":       protocol.OutcomeAuthFailed,
	"permission_denied": protocol.OutcomePermissionDenied,
	"overloaded":        protocol.OutcomeOverloaded,
	"server_error":      protocol.OutcomeServerError,
	"client_error":      protocol.OutcomeClientError,
}

// restAPIDefs 转换配置中的声明。配置已经过 config 包的静态校验（CFG-20）。
func restAPIDefs(cfg *config.Config) ([]rest.APIDef, error) {
	if cfg == nil {
		return nil, nil
	}
	poolProto := make(map[string]string, len(cfg.Pools))
	for _, p := range cfg.Pools {
		poolProto[p.Name] = p.Protocol
	}
	var defs []rest.APIDef
	for i, m := range cfg.Models {
		if m.API == nil {
			continue
		}
		if proto := poolProto[m.Pool]; proto != string(rest.Protocol) { // DA-01
			return nil, fmt.Errorf("config: models[%d].api: pool %q uses protocol %q, which does not support api declarations (only %q does)", i, m.Pool, proto, rest.Protocol)
		}
		d := rest.APIDef{Service: m.Name}
		for j, r := range m.API.Errors {
			kind, ok := apiOutcomeKinds[r.Outcome]
			if !ok {
				return nil, fmt.Errorf("config: models[%d].api.errors[%d].outcome: unknown %q", i, j, r.Outcome)
			}
			rule := rest.ErrorRule{Status: r.When.Status, In: r.When.In, NotIn: r.When.NotIn, Exists: r.When.Exists, Outcome: kind, Client: r.Status}
			if r.When.JSON != "" {
				p, err := config.ParseFieldPath(r.When.JSON)
				if err != nil {
					return nil, fmt.Errorf("config: models[%d].api.errors[%d].when.json: %w", i, j, err)
				}
				rule.JSON = p
			}
			if c := r.CooldownUntil; c != nil {
				h, mi, err := config.ParseClock(c.Time)
				if err != nil {
					return nil, fmt.Errorf("config: models[%d].api.errors[%d].cooldown_until: %w", i, j, err)
				}
				loc, err := config.ParseTZ(c.TZ)
				if err != nil {
					return nil, fmt.Errorf("config: models[%d].api.errors[%d].cooldown_until: %w", i, j, err)
				}
				rule.Cooldown = &rest.Cooldown{DayOfMonth: c.DayOfMonth, Hour: h, Minute: mi, Loc: loc}
			}
			d.Errors = append(d.Errors, rule)
		}
		for j, e := range m.API.Endpoints {
			method, segs, err := config.ParseAPIMatch(e.Match)
			if err != nil {
				return nil, fmt.Errorf("config: models[%d].api.endpoints[%d].match: %w", i, j, err)
			}
			ep := rest.EndpointDef{Method: method, Segments: segs, PinnedBy: e.PinnedBy}
			if e.TaskID != "" {
				if ep.TaskID, err = config.ParseFieldPath(e.TaskID); err != nil {
					return nil, fmt.Errorf("config: models[%d].api.endpoints[%d].task_id: %w", i, j, err)
				}
			}
			if e.Usage != nil {
				ep.Usage = make(map[string]rest.UsageRule, len(e.Usage))
				for unit, u := range e.Usage {
					r := rest.UsageRule{RequestQuery: u.RequestQuery, Map: u.Map, Default: u.Default}
					if u.JSON != "" {
						if r.JSON, err = config.ParseFieldPath(u.JSON); err != nil {
							return nil, fmt.Errorf("config: models[%d].api.endpoints[%d].usage.%s.json: %w", i, j, unit, err)
						}
					}
					if u.RequestJSON != "" {
						if r.RequestJSON, err = config.ParseFieldPath(u.RequestJSON); err != nil {
							return nil, fmt.Errorf("config: models[%d].api.endpoints[%d].usage.%s.request_json: %w", i, j, unit, err)
						}
					}
					if z := u.ZipJSON; z != nil {
						count, err := config.ParseFieldPath(z.Count)
						if err != nil {
							return nil, fmt.Errorf("config: models[%d].api.endpoints[%d].usage.%s.zip_json.count: %w", i, j, unit, err)
						}
						r.ZipJSON = &rest.ZipCount{Entry: z.Entry, Count: count}
					}
					ep.Usage[unit] = r
				}
			}
			if e.Flow != nil {
				fl, err := restFlowDef(e.Flow)
				if err != nil {
					return nil, fmt.Errorf("config: models[%d].api.endpoints[%d].flow: %w", i, j, err)
				}
				ep.Flow = fl
			}
			d.Endpoints = append(d.Endpoints, ep)
		}
		defs = append(defs, d)
	}
	return defs, nil
}

// withRestUnitPrices 返回给内核使用的配置副本：rest 池上的服务对 units 中未定价的单位补 "0"（DA-13）。
// 服务永远不会报告别的服务声明的单位，所以补 0 不会少计费；自己声明的单位必须显式定价（CFG-20）。
// 不修改 cfg 本身（管理面展示的仍是用户写的配置）。
func withRestUnitPrices(cfg *config.Config, units []string) *config.Config {
	if cfg == nil {
		return nil
	}
	restPools := map[string]bool{}
	for _, p := range cfg.Pools {
		if p.Protocol == string(rest.Protocol) {
			restPools[p.Name] = true
		}
	}
	out := *cfg
	out.Models = make([]config.ModelConfig, len(cfg.Models))
	for i, m := range cfg.Models {
		if restPools[m.Pool] {
			price := maps.Clone(m.Price)
			if price == nil {
				price = map[string]string{}
			}
			for _, u := range units {
				if _, ok := price[u]; !ok && u != rest.UnitRequests {
					price[u] = "0"
				}
			}
			m.Price = price
		}
		out.Models[i] = m
	}
	return &out
}

// restUnits 返回已注册的 rest 出站适配器的计量单位；没有注册时为空。
func restUnits(reg *protocol.Registry) []string {
	if eg, ok := reg.Egress(rest.Protocol); ok {
		return eg.Units()
	}
	return nil
}

// restFlowDef 转换 flow（FL-01 ~ FL-06）。配置已经过 CFG-20 的静态校验。
func restFlowDef(f *config.APIFlow) (*rest.FlowDef, error) {
	step := func(req, taskID string) (rest.FlowStep, error) {
		m, segs, err := config.ParseAPIMatch(req)
		if err != nil {
			return rest.FlowStep{}, err
		}
		st := rest.FlowStep{Method: m, Path: "/" + strings.Join(segs, "/")}
		if taskID != "" {
			if st.TaskID, err = config.ParseFieldPath(taskID); err != nil {
				return rest.FlowStep{}, err
			}
		}
		return st, nil
	}
	out := &rest.FlowDef{}
	if s := f.Submit; s != nil {
		st, err := step(s.Request, s.TaskID)
		if err != nil {
			return nil, fmt.Errorf("submit: %w", err)
		}
		out.Submit = &st
	}
	if u := f.Upload; u != nil {
		st, err := step(u.Request, u.TaskID)
		if err != nil {
			return nil, fmt.Errorf("upload: %w", err)
		}
		up := &rest.FlowUpload{FlowStep: st}
		if up.UploadURL, err = config.ParseFieldPath(u.UploadURL); err != nil {
			return nil, fmt.Errorf("upload.upload_url: %w", err)
		}
		for target, src := range u.Body {
			segs, err := config.ParseFieldPath(target)
			if err != nil {
				return nil, fmt.Errorf("upload.body: %w", err)
			}
			typ := src.Type
			if typ == "" {
				typ = "string"
			}
			up.Body = append(up.Body, rest.BodyField{Target: segs, Query: src.Query, Type: typ, Required: src.Required})
		}
		out.Upload = up
	}
	if p := f.Poll; p != nil {
		m, segs, err := config.ParseAPIMatch(p.Request)
		if err != nil {
			return nil, fmt.Errorf("poll: %w", err)
		}
		state, err := config.ParseFieldPath(p.State)
		if err != nil {
			return nil, fmt.Errorf("poll.state: %w", err)
		}
		out.Poll = &rest.FlowPoll{Method: m, Path: "/" + strings.Join(segs, "/"), State: state, Done: p.Done, Failed: p.Failed, Timeout: p.Timeout}
	}
	if r := f.Result; r != nil {
		res := &rest.FlowResult{ZipEntry: r.ZipEntry, ContentType: r.ContentType}
		if r.URL != "" {
			var err error
			if res.URL, err = config.ParseFieldPath(r.URL); err != nil {
				return nil, fmt.Errorf("result.url: %w", err)
			}
		}
		out.Result = res
	}
	return out, nil
}
