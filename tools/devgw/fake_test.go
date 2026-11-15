package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnswer(t *testing.T) {
	cases := []struct {
		name       string
		r          fakeReq
		text, tool string
	}{
		{"default", fakeReq{lastRole: "user", lastText: "hi", lastUserParts: []string{"hi"}}, "pong", ""},
		{"echo", fakeReq{lastRole: "user", lastText: "echo: a b", lastUserParts: []string{"echo: a b"}}, " a b", ""},
		{"call", fakeReq{Tools: []string{"get_time"}, lastRole: "user", lastText: "please call:get_time.", lastUserParts: []string{"please call:get_time."}}, "", "get_time"},
		{"call unknown tool", fakeReq{Tools: []string{"x"}, lastRole: "user", lastText: "call:get_time"}, "pong", ""},
		{"call after tool result", fakeReq{Tools: []string{"get_time"}, lastRole: "tool", lastText: "call:get_time", lastUserParts: []string{"call:get_time"}}, "pong", ""},
		{"echo uses last user message", fakeReq{lastRole: "function_call_output", lastUserParts: []string{"echo:x"}}, "x", ""},
		{"echo in a later text block", fakeReq{lastRole: "user", lastUserParts: []string{"<system-reminder>r</system-reminder>", "echo:cc"}}, "cc", ""},
	}
	for _, c := range cases {
		text, tool := answer(c.r)
		if text != c.text || tool != c.tool {
			t.Errorf("%s: got %q %q", c.name, text, tool)
		}
	}
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// 假上游的流式与非流式格式、工具调用与 fake.log（经网关转发的检查见 TestDevgwEndToEnd）。
func TestFakeUpstreams(t *testing.T) {
	var logBuf bytes.Buffer
	log := &fakeLog{w: &logBuf}
	h := map[string]http.Handler{}
	for _, n := range []string{"openai", "anthropic", "responses"} {
		var err error
		if h[n], err = newFakeHandler(n, log); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newFakeHandler("gemini", log); err == nil {
		t.Fatal("unknown fake accepted")
	}
	type check struct {
		name, proto, path, body string
		want                    []string
	}
	checks := []check{
		{"openai", "openai", "/v1/chat/completions", `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`,
			[]string{`"content":"pong"`, `"finish_reason":"stop"`, `"prompt_tokens":12`}},
		{"openai stream usage", "openai", "/v1/chat/completions", `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":[{"type":"text","text":"echo:hello"}]}]}`,
			[]string{`"content":"hello"`, `"usage":{`, "data: [DONE]\n\n"}},
		{"openai tool", "openai", "/v1/chat/completions", `{"model":"m","tools":[{"type":"function","function":{"name":"get_time"}}],"messages":[{"role":"user","content":"call:get_time"}]}`,
			[]string{`"tool_calls":[{`, `"name":"get_time"`, `"finish_reason":"tool_calls"`}},
		{"openai stream tool", "openai", "/v1/chat/completions", `{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"get_time"}}],"messages":[{"role":"user","content":"call:get_time"}]}`,
			[]string{`"name":"get_time"`, `"arguments":"{}"`, `"finish_reason":"tool_calls"`}},
		{"anthropic", "anthropic", "/v1/messages", `{"model":"m","system":"s","messages":[{"role":"user","content":"hi"}]}`,
			[]string{`"text":"pong","type":"text"`, `"stop_reason":"end_turn"`}},
		{"anthropic stream", "anthropic", "/v1/messages", `{"model":"m","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"echo:yo"}]}]}`,
			[]string{"event: message_start\n", `"text":"yo","type":"text_delta"`, "event: message_stop\n"}},
		{"anthropic stream tool", "anthropic", "/v1/messages", `{"model":"m","stream":true,"tools":[{"name":"get_time"}],"messages":[{"role":"user","content":"call:get_time"}]}`,
			[]string{`"type":"tool_use"`, `"input_json_delta"`, `"stop_reason":"tool_use"`}},
		{"anthropic count", "anthropic", "/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			[]string{`{"input_tokens":12}`}},
		{"responses", "responses", "/v1/responses", `{"model":"m","input":"hi"}`,
			[]string{`"type":"output_text"`, `"text":"pong"`, `"status":"completed"`}},
		{"responses stream", "responses", "/v1/responses", `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"echo:z"}]}]}`,
			[]string{"event: response.created\n", `"delta":"z"`, `"content":[]`, "event: response.completed\n"}},
		{"responses tool", "responses", "/v1/responses", `{"model":"m","stream":true,"tools":[{"type":"function","name":"get_time"}],"input":[{"role":"user","content":"call:get_time"}]}`,
			[]string{`"type":"function_call"`, "response.function_call_arguments.done"}},
	}
	for _, c := range checks {
		rec := post(t, h[c.proto], c.path, c.body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: HTTP %d", c.name, rec.Code)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(rec.Body.String(), w) {
				t.Errorf("%s: missing %q in %s", c.name, w, rec.Body.String())
			}
		}
	}
	if rec := post(t, h["openai"], "/v1/chat/completions", "{bad"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d", rec.Code)
	}

	// fake.log：每个请求一行，有角色序列，没有内容
	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != len(checks) {
		t.Fatalf("fake.log has %d lines, want %d", len(lines), len(checks))
	}
	var first struct {
		Path  string   `json:"path"`
		Roles []string `json:"roles"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Path != "/v1/chat/completions" || strings.Join(first.Roles, ",") != "system,user" {
		t.Errorf("fake.log line: %s (%v)", lines[0], err)
	}
	if strings.Contains(logBuf.String(), "hello") || strings.Contains(logBuf.String(), "echo:") {
		t.Error("fake.log contains message content")
	}
}
