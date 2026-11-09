package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

func testChatTranslator() ChatToAnthropic {
	return ChatToAnthropic{Now: func() time.Time { return fixedTime }, NewID: seqIDs()}
}

// 黄金文件的流式用例请求了 usage chunk，用来覆盖 §5.2 message_stop 的完整输出。
var chatEnv = &protocol.Envelope{Protocol: protoOpenAIChat, Op: opChatCompletions, Model: "claude-for-chat",
	Body: []byte(`{"model":"claude-for-chat","stream":true,"stream_options":{"include_usage":true}}`)}

// encodeChatSSE 每个 chunk 输出缩进的 JSON（[DONE] 原样输出），便于人工核对黄金文件。
func encodeChatSSE(evs []sse.Event) []byte {
	var b bytes.Buffer
	for _, ev := range evs {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, ev.Data, "", "  "); err != nil {
			pretty.Write(ev.Data)
		}
		for _, line := range strings.Split(pretty.String(), "\n") {
			b.WriteString("data: " + line + "\n")
		}
		b.WriteString("\n")
	}
	return b.Bytes()
}

func drainChat(t *testing.T, s protocol.EventStream) []sse.Event {
	t.Helper()
	var got []sse.Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		got = append(got, ev)
	}
}

// spec/10 §5.1 + §7：testdata/anthropic_to_chat/<用例>/{input.json, expected.json}。
func TestAnthropicToChatBufferedGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "anthropic_to_chat", "*", "input.json"))
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
			out, err := testChatTranslator().Response(chatEnv, &protocol.Response{Kind: protocol.ResponseBuffered, Status: 200, Body: body})
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

// spec/10 §5.2 + §7：testdata/anthropic_to_chat/<用例>/{input.sse, expected.sse}。
func TestAnthropicToChatStreamGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "anthropic_to_chat", "*", "input.sse"))
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
			out, err := testChatTranslator().Response(chatEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
			if err != nil {
				t.Fatalf("Response() error = %v", err)
			}
			got := drainChat(t, out.Events)
			checkChatChunks(t, got)
			if err := out.Events.Close(); err != nil || !upstream.closed {
				t.Errorf("Close() did not close upstream (err=%v)", err)
			}
			compareGolden(t, filepath.Join(dir, "expected.sse"), encodeChatSSE(got))
		})
	}
}

// checkChatChunks 校验 §5.2 的通用约束：没有 event 名；所有 chunk 的 id/created/model 相同；以 [DONE] 结束。
func checkChatChunks(t *testing.T, evs []sse.Event) {
	t.Helper()
	if len(evs) == 0 || string(evs[len(evs)-1].Data) != "[DONE]" {
		t.Fatalf("stream does not end with [DONE]")
	}
	var first struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
	}
	for i, ev := range evs[:len(evs)-1] {
		var c = first
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if i == 0 {
			first = c
		}
		if ev.Name != "" || c != first || c.Object != "chat.completion.chunk" || !strings.HasPrefix(c.ID, "chatcmpl-") {
			t.Errorf("chunk %d: name=%q %+v (first %+v)", i, ev.Name, c, first)
		}
	}
}

// 没有请求 stream_options.include_usage 时不输出 usage chunk，finish chunk 之后直接 [DONE]。
func TestAnthropicToChatStreamWithoutUsage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "anthropic_to_chat", "01_text", "input.sse"))
	if err != nil {
		t.Fatal(err)
	}
	env := &protocol.Envelope{Op: opChatCompletions, Model: "m", Body: []byte(`{"stream":true}`)}
	out, err := testChatTranslator().Response(env, &protocol.Response{Kind: protocol.ResponseEvents, Events: &sliceStream{evs: readSSE(t, raw)}})
	if err != nil {
		t.Fatal(err)
	}
	got := drainChat(t, out.Events)
	if len(got) < 2 || string(got[len(got)-1].Data) != "[DONE]" || !strings.Contains(string(got[len(got)-2].Data), `"finish_reason":"stop"`) {
		t.Fatalf("tail = %s", encodeChatSSE(got[max(0, len(got)-2):]))
	}
	for _, ev := range got {
		if strings.Contains(string(ev.Data), `"usage"`) {
			t.Errorf("unexpected usage chunk %s", ev.Data)
		}
	}
}

// 上游 error 事件、上游流在 message_stop 之前结束：返回 *AttemptError，内核据此写入站 StreamError（不发 [DONE]）。
func TestAnthropicToChatStreamErrors(t *testing.T) {
	start := sse.Event{Name: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}`)}
	tests := []struct {
		name     string
		upstream *sliceStream
		wantKind protocol.OutcomeKind
	}{
		{"error event", &sliceStream{evs: []sse.Event{start,
			{Name: "error", Data: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`)}}},
			protocol.OutcomeRateLimited},
		{"truncated", &sliceStream{evs: []sse.Event{start}}, protocol.OutcomeNetworkError},
		{"upstream read error", &sliceStream{evs: []sse.Event{start},
			end: &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeTimeout}}}, protocol.OutcomeTimeout},
		{"bad json", &sliceStream{evs: []sse.Event{{Name: "content_block_delta", Data: []byte(`{`)}}}, protocol.OutcomeServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := testChatTranslator().Response(chatEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: tt.upstream})
			if err != nil {
				t.Fatal(err)
			}
			var last error
			for range 20 {
				var ev sse.Event
				if ev, last = out.Events.Next(); last != nil {
					break
				}
				if string(ev.Data) == "[DONE]" {
					t.Fatal("[DONE] emitted on a failed stream")
				}
			}
			var ae *protocol.AttemptError
			if !errors.As(last, &ae) || ae.Outcome.Kind != tt.wantKind {
				t.Fatalf("final error = %v, want AttemptError kind %v", last, tt.wantKind)
			}
		})
	}
}

// §5.1 stop_reason 映射，含 SPEC-GAP(T22) 的兜底。
func TestFinishReasonFor(t *testing.T) {
	for in, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls",
		"refusal": "content_filter", "pause_turn": "stop", "": "stop",
	} {
		if got := finishReasonFor(in); got != want {
			t.Errorf("finishReasonFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// §5.1 usage：prompt = input + cache_read + cache_write，cached_tokens = cache_read；id 前缀改为 chatcmpl-。
func TestAnthropicToChatUsageAndID(t *testing.T) {
	body := []byte(`{"id":"msg_abc","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}],"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":7}}`)
	b, err := anthropicToChatBody(body, "m", fixedTime, seqIDs())
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	u := out.Usage
	if out.ID != "chatcmpl-abc" || u.PromptTokens != 115 || u.CompletionTokens != 7 || u.TotalTokens != 122 || u.PromptTokensDetails.CachedTokens != 100 {
		t.Errorf("id=%q usage=%+v", out.ID, u)
	}
	if m := out.Choices[0].Message; m.Content != nil || m.ToolCalls[0].Function.Arguments != "{}" {
		t.Errorf("message = %s", b)
	}
	if _, err := anthropicToChatBody([]byte(`{`), "m", fixedTime, seqIDs()); err == nil {
		t.Error("bad json accepted")
	}
	if got := chatID("", seqIDs()); got != "chatcmpl-1" {
		t.Errorf("chatID(\"\") = %q", got)
	}
	if got := chatID("abc", seqIDs()); got != "chatcmpl-abc" {
		t.Errorf("chatID(abc) = %q", got)
	}
}

func TestIncludeUsage(t *testing.T) {
	for body, want := range map[string]bool{
		`{"stream_options":{"include_usage":true}}`:  true,
		`{"stream_options":{"include_usage":false}}`: false,
		`{"stream_options":null}`:                    false,
		`{}`:                                         false,
		`{`:                                          false,
	} {
		if got := includeUsage([]byte(body)); got != want {
			t.Errorf("includeUsage(%s) = %v", body, got)
		}
	}
}

// Request：Chat → IR → Anthropic；不支持的字段与非 chat.completions 操作 → 400。
func TestChatToAnthropicRequest(t *testing.T) {
	tr := ChatToAnthropic{}
	if tr.From() != protoOpenAIChat || tr.To() != protoAnthropic {
		t.Fatalf("From/To = %s/%s", tr.From(), tr.To())
	}
	env := &protocol.Envelope{Op: opChatCompletions, Model: "claude-for-chat", Stream: true,
		Body: []byte(`{"model":"claude-for-chat","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"}]}`)}
	call, err := tr.Request(env, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if call.Op != opAnthropicCreate || call.Model != "claude-sonnet-4-5" || !call.Stream {
		t.Errorf("call = %+v", call)
	}
	var body struct {
		Model     string            `json:"model"`
		MaxTokens int64             `json:"max_tokens"`
		Stream    bool              `json:"stream"`
		Other     map[string]any    `json:"stream_options"`
		System    []json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "claude-sonnet-4-5" || body.MaxTokens != DefaultMaxTokens || !body.Stream || body.Other != nil || len(body.System) != 1 {
		t.Errorf("anthropic body = %s", call.Body)
	}

	for name, env := range map[string]*protocol.Envelope{
		"n>1":        {Op: opChatCompletions, Model: "m", Body: []byte(`{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`)},
		"embeddings": {Op: "embeddings", Model: "m"},
		"invalid ir": {Op: opChatCompletions, Model: "m", Body: []byte(`{"model":"m","messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"[1]"}}]}]}`)},
	} {
		if _, err := tr.Request(env, "up"); !isInvalid(err) {
			t.Errorf("%s: error = %v, want invalid_request", name, err)
		}
	}
}

// 字节流原样返回；缓冲体解析失败返回错误。
func TestChatToAnthropicResponseEdges(t *testing.T) {
	in := &protocol.Response{Kind: protocol.ResponseRaw}
	if out, err := (ChatToAnthropic{}).Response(chatEnv, in); err != nil || out != in {
		t.Fatalf("Response(raw) = %v, %v", out, err)
	}
	if _, err := (ChatToAnthropic{}).Response(chatEnv, &protocol.Response{Kind: protocol.ResponseBuffered, Body: []byte(`x`)}); err == nil {
		t.Error("bad buffered body accepted")
	}
}

// Register 注册全部转换器；重复注册返回错误。
func TestRegister(t *testing.T) {
	r := protocol.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	for _, from := range []protocol.Protocol{protoOpenAIChat, protoOpenAIResponses} {
		if _, ok := r.Translator(from, protoAnthropic); !ok {
			t.Errorf("translator %s -> anthropic not registered", from)
		}
	}
	if _, ok := r.Translator(protoAnthropic, protoOpenAIChat); !ok {
		t.Error("translator anthropic -> openai_chat not registered")
	}
	if err := Register(r); err == nil {
		t.Error("second Register() succeeded")
	}
}
