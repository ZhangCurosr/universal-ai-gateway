package config

import (
	"strings"
	"testing"
)

// apiYAML 是带声明式 API 能力的配置（CFG-20）；%s 处插入 api 块（缩进 4 格）。
const apiYAML = `
server:
  data_dir: "./data"
  master_key_file: "./secrets/master.key"
pools:
  - name: rp
    protocol: rest
    base_url: "https://mineru.net"
models:
  - name: mu
    pool: rp
    price: { pages: "0.01", requests: "0" }
    reserve: { pages: 600 }
    api:
%s
`

const goodAPI = `      errors:
        - when: { json: msgCode, in: ["A0202", "A0211"] }
          outcome: auth_failed
        - when: { json: code, in: [-60018, -60019] }
          outcome: rate_limited
          cooldown_until: { time: "00:01", tz: "+08:00" }
        - when: { json: code, in: [-60012] }
          outcome: client_error
          status: 404
        - when: { json: code, not_in: [0] }
          outcome: client_error
        - when: { status: [432, 433] }
          outcome: rate_limited
          cooldown_until: { day_of_month: 1, time: "00:00" }
      endpoints:
        - match: "post /api/v4/extract/task"
          task_id: data.task_id
          usage: { pages: { request_query: pages, default: 600 } }
        - match: "GET /api/v4/extract/task/{task}"
          pinned_by: task
          usage: {}
        - match: "GET /api/v4/other"
`

func apiCfg(api string) string { return strings.Replace(apiYAML, "%s", api, 1) }

func TestAPIConfigParses(t *testing.T) {
	cfg, err := Parse([]byte(apiCfg(goodAPI)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	a := cfg.Models[0].API
	if len(a.Errors) != 5 || len(a.Endpoints) != 3 {
		t.Fatalf("api = %+v", a)
	}
	if a.Errors[1].When.In[0] != -60018 || a.Errors[0].When.In[0] != "A0202" {
		t.Errorf("in values = %#v %#v", a.Errors[1].When.In, a.Errors[0].When.In)
	}
	if a.Endpoints[1].Usage == nil || len(a.Endpoints[1].Usage) != 0 {
		t.Error("usage: {} must be an empty, non-nil map (no billing)")
	}
	if a.Endpoints[2].Usage != nil {
		t.Error("absent usage must stay nil (billed per request)")
	}
	if *a.Endpoints[0].Usage["pages"].Default != 600 {
		t.Error("default not parsed")
	}
	m, segs, err := ParseAPIMatch(a.Endpoints[0].Match)
	if err != nil || m != "POST" || strings.Join(segs, "/") != "api/v4/extract/task" {
		t.Errorf("ParseAPIMatch = %q %v %v", m, segs, err)
	}
}

func TestAPIConfigRejects(t *testing.T) {
	ep := func(extra string) string { return "      endpoints:\n        - match: \"GET /t/{id}\"\n" + extra }
	er := func(rule string) string { return "      errors:\n        - " + rule + "\n" }
	tests := []struct{ name, api, want string }{
		{"empty when", er("{ when: {}, outcome: auth_failed }"), "needs status or json"},
		{"bad status", er("{ when: { status: [99] }, outcome: auth_failed }"), "HTTP status"},
		{"json without test", er("{ when: { json: code }, outcome: auth_failed }"), "in, not_in or exists"},
		{"in without json", er("{ when: { status: [200], in: [1] }, outcome: auth_failed }"), "need json"},
		{"bad path", er("{ when: { json: a..b, in: [1] }, outcome: auth_failed }"), "empty segment"},
		{"non-scalar in", er("{ when: { json: c, in: [[1]] }, outcome: auth_failed }"), "strings, numbers or booleans"},
		{"bad outcome", er("{ when: { status: [500] }, outcome: retry }"), "outcome"},
		{"status on rate_limited", er("{ when: { status: [500] }, outcome: rate_limited, status: 429 }"), "only allowed for client_error"},
		{"status out of range", er("{ when: { status: [500] }, outcome: client_error, status: 500 }"), "400 ~ 499"},
		{"cooldown on auth", er("{ when: { status: [500] }, outcome: auth_failed, cooldown_until: { time: \"00:00\" } }"), "only allowed for rate_limited"},
		{"bad clock", er("{ when: { status: [429] }, outcome: rate_limited, cooldown_until: { time: \"24:00\" } }"), "HH:MM"},
		{"bad day", er("{ when: { status: [429] }, outcome: rate_limited, cooldown_until: { time: \"00:00\", day_of_month: 31 } }"), "1 ~ 28"},
		{"bad tz", er("{ when: { status: [429] }, outcome: rate_limited, cooldown_until: { time: \"00:00\", tz: Asia/Shanghai } }"), "time zone"},
		{"bad method", "      endpoints:\n        - match: \"OPTIONS /x\"\n", "match"},
		{"no path", "      endpoints:\n        - match: \"GET\"\n", "match"},
		{"partial placeholder", "      endpoints:\n        - match: \"GET /t/{id}.json\"\n", "whole segment"},
		{"repeated placeholder", "      endpoints:\n        - match: \"GET /{id}/{id}\"\n", "more than once"},
		{"dot segment", "      endpoints:\n        - match: \"GET /a/../b\"\n", "invalid path segment"},
		{"query in match", "      endpoints:\n        - match: \"GET /a?b=1\"\n", "must not contain"},
		{"pinned_by unknown", ep("          pinned_by: task\n"), "not in match"},
		{"pinned and task_id", ep("          pinned_by: id\n          task_id: data.id\n"), "cannot be used together"},
		{"unpriced unit", ep("          usage: { credits: { default: 1 } }\n"), "must be priced"},
		{"invalid unit", ep("          usage: { \"Bad Unit\": { default: 1 } }\n"), "invalid unit"},
		{"empty usage rule", ep("          usage: { pages: {} }\n"), "needs json"},
		{"two request sources", ep("          usage: { pages: { request_json: a, request_query: b } }\n"), "not both"},
		{"map without request", ep("          usage: { pages: { json: a, map: { x: 1 } } }\n"), "only applies"},
		{"negative map", ep("          usage: { pages: { request_json: a, map: { x: -1 } } }\n"), ">= 0"},
		{"negative default", ep("          usage: { pages: { default: -1 } }\n"), ">= 0"},
		{"unknown field", "      retries: 3\n", "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(apiCfg(tt.api)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseTZAndClock(t *testing.T) {
	for _, s := range []string{"", "UTC", "+08:00", "-05:30"} {
		if _, err := ParseTZ(s); err != nil {
			t.Errorf("ParseTZ(%q): %v", s, err)
		}
	}
	for _, s := range []string{"+8", "+15:00", "Asia/Shanghai", "+08:60"} {
		if _, err := ParseTZ(s); err == nil {
			t.Errorf("ParseTZ(%q) accepted", s)
		}
	}
	if h, m, err := ParseClock("23:59"); err != nil || h != 23 || m != 59 {
		t.Errorf("ParseClock = %d %d %v", h, m, err)
	}
	for _, s := range []string{"9:00", "24:00", "12:60", "noon"} {
		if _, _, err := ParseClock(s); err == nil {
			t.Errorf("ParseClock(%q) accepted", s)
		}
	}
	if segs, err := ParseFieldPath("a.0.b"); err != nil || len(segs) != 3 {
		t.Errorf("ParseFieldPath = %v %v", segs, err)
	}
	if _, err := ParseFieldPath(""); err == nil {
		t.Error("empty path accepted")
	}
}
