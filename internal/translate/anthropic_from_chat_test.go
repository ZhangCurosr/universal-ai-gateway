package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

func testAnthropicTranslator() AnthropicToChat { return AnthropicToChat{NewID: seqIDs()} }

var anthropicEnv = &protocol.Envelope{Protocol: protoAnthropic, Op: opAnthropicCreate, Model: "deepseek-for-claude"}

// spec/10 §6.3 非流式 + §6.4：testdata/chat_to_anthropic/<用例>/{input.json, expected.json}。
func TestChatToAnthropicBufferedGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "chat_to_anthropic", "*", "input.json"))
	if len(cases) == 0 {
		t.Fatal("no buffered golden cases")
	}
	for _, in := range cases {
		dir := filepath.Dir(in)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			body, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			out, err := testAnthropicTranslator().Response(anthropicEnv, &protocol.Response{Kind: protocol.ResponseBuffered, Status: 200, Body: body})
			if err != nil {
				t.Fatalf("Response() error = %v", err)
			}
			if ct := out.Header.Get("Content-Type"); ct != "application/json" || out.Status != 200 {
				t.Errorf("Content-Type = %q status = %d", ct, out.Status)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, out.Body, "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			compareGolden(t, filepath.Join(dir, "expected.json"), pretty.Bytes())
		})
	}
}

// spec/10 §6.3 流式 + §6.4：testdata/chat_to_anthropic/<用例>/{input.sse, expected.sse}。
func TestChatToAnthropicStreamGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "chat_to_anthropic", "*", "input.sse"))
	if len(cases) == 0 {
		t.Fatal("no stream golden cases")
	}
	for _, in := range cases {
		dir := filepath.Dir(in)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			upstream := &sliceStream{evs: readSSE(t, raw)}
			out, err := testAnthropicTranslator().Response(anthropicEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
			if err != nil {
				t.Fatalf("Response() error = %v", err)
			}
			got := drainChat(t, out.Events)
			checkAnthropicEvents(t, got)
			if err := out.Events.Close(); err != nil || !upstream.closed {
				t.Errorf("Close() did not close upstream (err=%v)", err)
			}
			compareGolden(t, filepath.Join(dir, "expected.sse"), encodeSSE(got))
		})
	}
}

// checkAnthropicEvents 校验通用约束：event 名与 data.type 相同；message_start 开头、message_stop 结尾；
// 每个 content_block_start 都有匹配的 content_block_stop，index 从 0 连续递增。
func checkAnthropicEvents(t *testing.T, evs []sse.Event) {
	t.Helper()
	if len(evs) < 2 || evs[0].Name != "message_start" || evs[len(evs)-1].Name != "message_stop" {
		t.Fatalf("bad framing: %d events", len(evs))
	}
	open, next := -1, 0
	for i, ev := range evs {
		var d struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if d.Type != ev.Name {
			t.Errorf("event %d: name=%q type=%q", i, ev.Name, d.Type)
		}
		switch ev.Name {
		case "content_block_start":
			if open != -1 || d.Index != next {
				t.Errorf("event %d: start index %d while open=%d next=%d", i, d.Index, open, next)
			}
			open, next = d.Index, next+1
		case "content_block_delta":
			if d.Index != open {
				t.Errorf("event %d: delta index %d, open block %d", i, d.Index, open)
			}
		case "content_block_stop":
			if d.Index != open {
				t.Errorf("event %d: stop index %d, open block %d", i, d.Index, open)
			}
			open = -1
		}
	}
	if open != -1 {
		t.Errorf("block %d never closed", open)
	}
}

func chunkEv(s string) sse.Event { return sse.Event{Data: []byte(s)} }

// 上游错误 chunk、截断、[DONE] 前没有 finish_reason、工具参数交错：返回 *AttemptError（内核写 event: error）。
func TestChatToAnthropicStreamErrors(t *testing.T) {
	first := chunkEv(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`)
	tests := []struct {
		name     string
		upstream *sliceStream
		wantKind protocol.OutcomeKind
	}{
		{"error chunk", &sliceStream{evs: []sse.Event{first, chunkEv(`{"error":{"message":"boom","type":"server_error"}}`)}}, protocol.OutcomeServerError},
		{"truncated", &sliceStream{evs: []sse.Event{first}}, protocol.OutcomeNetworkError},
		{"done without finish", &sliceStream{evs: []sse.Event{first, chunkEv(`[DONE]`)}}, protocol.OutcomeServerError},
		{"upstream read error", &sliceStream{evs: []sse.Event{first},
			end: &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeTimeout}}}, protocol.OutcomeTimeout},
		{"bad json", &sliceStream{evs: []sse.Event{chunkEv(`{`)}}, protocol.OutcomeServerError},
		{"unrecognised finish_reason", &sliceStream{evs: []sse.Event{first, chunkEv(`{"choices":[{"delta":{},"finish_reason":"insufficient_system_resource"}]}`)}}, protocol.OutcomeServerError},
		{"interleaved tool args", &sliceStream{evs: []sse.Event{
			chunkEv(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":""}}]}}]}`),
			chunkEv(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":""}}]}}]}`),
			chunkEv(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`),
		}}, protocol.OutcomeServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := testAnthropicTranslator().Response(anthropicEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: tt.upstream})
			if err != nil {
				t.Fatal(err)
			}
			var last error
			for range 30 {
				var ev sse.Event
				if ev, last = out.Events.Next(); last != nil {
					break
				}
				if ev.Name == "message_stop" {
					t.Fatal("message_stop emitted on a failed stream")
				}
			}
			var ae *protocol.AttemptError
			if !errors.As(last, &ae) || ae.Outcome.Kind != tt.wantKind {
				t.Fatalf("final error = %v, want AttemptError kind %v", last, tt.wantKind)
			}
		})
	}
}

// 非流式：choices 不是恰好 1 个、参数不是 JSON 对象 → upstream_error；JSON 非法 → 错误。
func TestChatToAnthropicBufferedErrors(t *testing.T) {
	for name, body := range map[string]string{
		"no choices":                 `{"choices":[]}`,
		"two choices":                `{"choices":[{"message":{}},{"message":{}}]}`,
		"bad args":                   `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"[1]"}}]},"finish_reason":"tool_calls"}]}`,
		"unrecognised finish_reason": `{"choices":[{"message":{},"finish_reason":"insufficient_system_resource"}]}`,
	} {
		_, err := chatToAnthropicBody([]byte(body), "m", seqIDs())
		if ae, ok := apperr.As(err); !ok || ae.Code != apperr.CodeUpstreamError {
			t.Errorf("%s: error = %v, want upstream_error", name, err)
		}
	}
	if _, err := (AnthropicToChat{}).Response(anthropicEnv, &protocol.Response{Kind: protocol.ResponseBuffered, Body: []byte(`x`)}); err == nil {
		t.Error("bad json accepted")
	}
}

func TestStopReasonFor(t *testing.T) {
	valid := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "function_call": "tool_use",
		"content_filter": "refusal",
	}
	for in, want := range valid {
		got, err := stopReasonFor(in)
		if err != nil {
			t.Errorf("stopReasonFor(%q) returned unexpected error: %v", in, err)
		}
		if got != want {
			t.Errorf("stopReasonFor(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"insufficient_system_resource", "", "unknown_custom"} {
		if got, err := stopReasonFor(in); err == nil {
			t.Errorf("stopReasonFor(%q) = %q, want error", in, got)
		} else if ae, ok := apperr.As(err); !ok || ae.Code != apperr.CodeUpstreamError {
			t.Errorf("stopReasonFor(%q) error = %v, want CodeUpstreamError", in, err)
		}
	}
}

func TestAnthropicMsgID(t *testing.T) {
	for in, want := range map[string]string{"chatcmpl-abc": "msg_abc", "abc": "msg_abc", "": "msg_1"} {
		if got := anthropicMsgID(in, seqIDs()); got != want {
			t.Errorf("anthropicMsgID(%q) = %q, want %q", in, got, want)
		}
	}
}

// Request：Anthropic → IR → Chat；不支持的字段与非 messages 操作 → 400。
func TestAnthropicToChatRequest(t *testing.T) {
	tr := AnthropicToChat{}
	if tr.From() != protoAnthropic || tr.To() != protoOpenAIChat {
		t.Fatalf("From/To = %s/%s", tr.From(), tr.To())
	}
	env := &protocol.Envelope{Op: opAnthropicCreate, Model: "deepseek-for-claude", Stream: true,
		Body: []byte(`{"model":"deepseek-for-claude","max_tokens":100,"stream":true,"system":"Be brief.","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"Hi"}]}`)}
	call, err := tr.Request(env, "deepseek-chat")
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if call.Op != opChatCompletions || call.Model != "deepseek-chat" || !call.Stream {
		t.Errorf("call = %+v", call)
	}
	var body struct {
		Model         string `json:"model"`
		MaxTokens     int64  `json:"max_tokens"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Thinking json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "deepseek-chat" || body.MaxTokens != 100 || !body.StreamOptions.IncludeUsage ||
		len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Thinking != nil {
		t.Errorf("chat body = %s", call.Body)
	}

	for name, env := range map[string]*protocol.Envelope{
		"top_k":        {Op: opAnthropicCreate, Model: "m", Body: []byte(`{"model":"m","max_tokens":1,"top_k":5,"messages":[{"role":"user","content":"hi"}]}`)},
		"count_tokens": {Op: "messages.count_tokens", Model: "m"},
		"too many stops": {Op: opAnthropicCreate, Model: "m",
			Body: []byte(`{"model":"m","max_tokens":1,"stop_sequences":["a","b","c","d","e"],"messages":[{"role":"user","content":"hi"}]}`)},
	} {
		if _, err := tr.Request(env, "up"); !isInvalid(err) {
			t.Errorf("%s: error = %v, want invalid_request", name, err)
		}
	}
}

func TestAnthropicToChatRawPassthrough(t *testing.T) {
	in := &protocol.Response{Kind: protocol.ResponseRaw}
	if out, err := (AnthropicToChat{}).Response(anthropicEnv, in); err != nil || out != in {
		t.Fatalf("Response(raw) = %v, %v", out, err)
	}
	if !strings.HasPrefix(newGatewayID("msg_"), "msg_") {
		t.Error("default id prefix")
	}
}
