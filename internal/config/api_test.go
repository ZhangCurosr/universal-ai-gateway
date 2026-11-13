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

const goodFlow = `      endpoints:
        - match: "POST /parse"
          flow:
            submit: { request: "POST /api/v4/extract/task", task_id: data.task_id }
            poll:
              request: "GET /api/v4/extract/task/{task_id}"
              state: data.state
              done: [done]
              failed: [failed]
              timeout: 20m
            result: { url: data.full_zip_url, zip_entry: full.md, content_type: "text/markdown" }
          usage: { pages: { json: data.extract_progress.total_pages, zip_json: { entry: layout.json, count: pdf_info } } }
        - match: "POST /parse/file"
          flow:
            upload:
              request: "POST /api/v4/file-urls/batch"
              body:
                files.0.name: { query: name, required: true }
                files.0.is_ocr: { query: is_ocr, type: bool }
              upload_url: data.file_urls.0
              task_id: data.batch_id
`

// T67：flow 的解析与 CFG-20 校验
func TestAPIFlowConfig(t *testing.T) {
	cfg, err := Parse([]byte(apiCfg(goodFlow)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	f := cfg.Models[0].API.Endpoints[0].Flow
	if z := cfg.Models[0].API.Endpoints[0].Usage["pages"].ZipJSON; z == nil || z.Entry != "layout.json" || z.Count != "pdf_info" {
		t.Fatalf("zip_json = %+v", z)
	}
	if f.Submit.TaskID != "data.task_id" || f.Poll.Timeout.Minutes() != 20 || f.Result.ZipEntry != "full.md" {
		t.Fatalf("flow = %+v", f)
	}
	up := cfg.Models[0].API.Endpoints[1].Flow.Upload
	if !up.Body["files.0.name"].Required || up.Body["files.0.is_ocr"].Type != "bool" {
		t.Fatalf("upload = %+v", up)
	}

	flow := func(match, body string) string {
		return "      endpoints:\n        - match: \"" + match + "\"\n          flow:\n" + body
	}
	sub := "            submit: { request: \"POST /x\", task_id: id }\n"
	poll := "            poll: { request: \"GET /x/{task_id}\", state: s, done: [ok] }\n"
	tests := []struct{ name, api, want string }{
		{"no first step", flow("POST /p", poll), "exactly one of submit or upload"},
		{"both steps", flow("POST /p", sub+"            upload: { request: \"POST /u\", upload_url: u }\n"), "exactly one"},
		{"endpoint task_id", "      endpoints:\n        - match: \"POST /p\"\n          task_id: id\n          flow:\n" + sub, "cannot be used with flow"},
		{"placeholder in submit", flow("POST /p", "            submit: { request: \"POST /x/{id}\" }\n"), "only allowed in poll"},
		{"bad step request", flow("POST /p", "            submit: { request: \"FETCH /x\" }\n"), "submit.request"},
		{"upload with GET", flow("GET /p", "            upload: { request: \"POST /u\", upload_url: u }\n"), "POST or PATCH"},
		{"upload without url", flow("POST /p", "            upload: { request: \"POST /u\" }\n"), "upload_url"},
		{"body without query", flow("POST /p", "            upload: { request: \"POST /u\", upload_url: u, body: { a: { type: bool } } }\n"), "query"},
		{"body bad type", flow("POST /p", "            upload: { request: \"POST /u\", upload_url: u, body: { a: { query: q, type: float } } }\n"), "string, bool, int or list"},
		{"poll without task id", flow("POST /p", "            submit: { request: \"POST /x\" }\n"+poll), "poll needs task_id"},
		{"poll without placeholder", flow("POST /p", sub+"            poll: { request: \"GET /x\", state: s, done: [ok] }\n"), "must contain {task_id}"},
		{"poll other placeholder", flow("POST /p", sub+"            poll: { request: \"GET /x/{id}\", state: s, done: [ok] }\n"), "only the {task_id}"},
		{"poll without done", flow("POST /p", sub+"            poll: { request: \"GET /x/{task_id}\", state: s }\n"), "done"},
		{"poll timeout too long", flow("POST /p", sub+"            poll: { request: \"GET /x/{task_id}\", state: s, done: [ok], timeout: 2h }\n"), "1h"},
		{"result without poll", flow("POST /p", sub+"            result: { url: u }\n"), "needs poll"},
		{"zip without url", flow("POST /p", sub+poll+"            result: { zip_entry: a.md }\n"), "zip_entry"},
		{"zip entry with dir", flow("POST /p", sub+poll+"            result: { url: u, zip_entry: a/b.md }\n"), "plain file name"},
		{"zip_json without flow", "      endpoints:\n        - match: \"GET /x\"\n          usage: { pages: { zip_json: { entry: l.json, count: n } } }\n", "zip_entry"},
		{"zip_json without zip result", flow("POST /p", sub+poll+"            result: { url: u }\n") + "          usage: { pages: { zip_json: { entry: l.json, count: n } } }\n", "zip_entry"},
		{"zip_json bad entry", flow("POST /p", sub+poll+"            result: { url: u, zip_entry: a.md }\n") + "          usage: { pages: { zip_json: { entry: d/l.json, count: n } } }\n", "plain file name"},
		{"zip_json bad count", flow("POST /p", sub+poll+"            result: { url: u, zip_entry: a.md }\n") + "          usage: { pages: { zip_json: { entry: l.json, count: \"a..b\" } } }\n", "zip_json.count"},
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
