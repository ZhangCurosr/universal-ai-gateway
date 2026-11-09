package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

func testResponsesChatTranslator() ResponsesToChat {
	return ResponsesToChat{Now: func() time.Time { return fixedTime }, NewID: seqIDs()}
}

var responsesChatEnv = &protocol.Envelope{Protocol: protoOpenAIResponses, Op: opResponsesCreate, Model: "deepseek-for-codex"}

// spec/10 §8.3 非流式（自 Chat）+ §8.4：testdata/chat_to_responses/<用例>/{input.json, expected.json}。
func TestChatToResponsesBufferedGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "chat_to_responses", "*", "input.json"))
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
			out, err := testResponsesChatTranslator().Response(responsesChatEnv, &protocol.Response{Kind: protocol.ResponseBuffered, Status: 200, Body: body})
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

// spec/10 §8.3 流式（自 Chat）+ §8.4：testdata/chat_to_responses/<用例>/{input.sse, expected.sse}。
func TestChatToResponsesStreamGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "chat_to_responses", "*", "input.sse"))
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
			out, err := testResponsesChatTranslator().Response(responsesChatEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
			if err != nil {
				t.Fatalf("Response() error = %v", err)
			}
			got := drainChat(t, out.Events)
			checkSequence(t, got)
			if err := out.Events.Close(); err != nil || !upstream.closed {
				t.Errorf("Close() did not close upstream (err=%v)", err)
			}
			compareGolden(t, filepath.Join(dir, "expected.sse"), encodeSSE(got))
		})
	}
}

// 上游错误 chunk、截断、[DONE] 前没有 finish_reason、工具参数交错：返回 *AttemptError，不输出 response.completed。
func TestChatToResponsesStreamErrors(t *testing.T) {
	first := chunkEv(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`)
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
		{"interleaved tool args", &sliceStream{evs: []sse.Event{
			chunkEv(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":""}}]}}]}`),
			chunkEv(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":""}}]}}]}`),
			chunkEv(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`),
		}}, protocol.OutcomeServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := testResponsesChatTranslator().Response(responsesChatEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: tt.upstream})
			if err != nil {
				t.Fatal(err)
			}
			var last error
			for range 40 {
				var ev sse.Event
				if ev, last = out.Events.Next(); last != nil {
					break
				}
				if ev.Name == "response.completed" {
					t.Fatal("response.completed emitted on a failed stream")
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
func TestChatToResponsesBufferedErrors(t *testing.T) {
	for name, body := range map[string]string{
		"no choices": `{"choices":[]}`,
		"bad args":   `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"x"}}]},"finish_reason":"tool_calls"}]}`,
	} {
		_, err := chatToResponsesBody([]byte(body), "m", fixedTime, seqIDs())
		if ae, ok := apperr.As(err); !ok || ae.Code != apperr.CodeUpstreamError {
			t.Errorf("%s: error = %v, want upstream_error", name, err)
		}
	}
	if _, err := (ResponsesToChat{}).Response(responsesChatEnv, &protocol.Response{Kind: protocol.ResponseBuffered, Body: []byte(`x`)}); err == nil {
		t.Error("bad json accepted")
	}
}

// Request：Responses → IR → Chat；RX-01、内置工具与非 create 操作 → 400。
func TestResponsesToChatRequest(t *testing.T) {
	tr := ResponsesToChat{}
	if tr.From() != protoOpenAIResponses || tr.To() != protoOpenAIChat {
		t.Fatalf("From/To = %s/%s", tr.From(), tr.To())
	}
	env := &protocol.Envelope{Op: opResponsesCreate, Model: "deepseek-for-codex", Stream: true,
		Body: []byte(`{"model":"deepseek-for-codex","instructions":"Be brief.","input":"Hi","stream":true,"max_output_tokens":100}`)}
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
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "deepseek-chat" || body.MaxTokens != 100 || !body.StreamOptions.IncludeUsage || len(body.Messages) != 2 ||
		body.Messages[0].Role != "system" || body.Messages[0].Content != "Be brief." || body.Messages[1].Content != "Hi" {
		t.Errorf("chat body = %s", call.Body)
	}

	for name, env := range map[string]*protocol.Envelope{
		"previous_response_id": {Op: opResponsesCreate, Model: "m", Body: []byte(`{"model":"m","input":"hi","previous_response_id":"resp_1"}`)},
		"built-in tool":        {Op: opResponsesCreate, Model: "m", Body: []byte(`{"model":"m","input":"hi","tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`)},
		"get by id (RX-04)":    {Op: "responses.get", Model: "m"},
	} {
		if _, err := tr.Request(env, "up"); !isInvalid(err) {
			t.Errorf("%s: error = %v, want invalid_request", name, err)
		}
	}
}

func TestResponsesToChatRawPassthrough(t *testing.T) {
	in := &protocol.Response{Kind: protocol.ResponseRaw}
	if out, err := (ResponsesToChat{}).Response(responsesChatEnv, in); err != nil || out != in {
		t.Fatalf("Response(raw) = %v, %v", out, err)
	}
}
