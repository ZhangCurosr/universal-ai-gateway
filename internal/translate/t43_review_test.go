package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// T43 验收：工具参数原样透传到 Anthropic 客户端，不重排键、不损失大整数精度；空参数为 {}（B12）。
func TestChatToAnthropicToolArgsVerbatim(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-x","choices":[{"message":{"tool_calls":[` +
		`{"id":"c1","function":{"name":"f","arguments":"{\"b\":1,\"a\":12345678901234567890}"}},` +
		`{"id":"c2","function":{"name":"g","arguments":""}}]},"finish_reason":"tool_calls"}]}`)
	out, err := chatToAnthropicBody(body, "m", seqIDs())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"input":{"b":1,"a":12345678901234567890}`, `"name":"g","input":{}`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output missing %s\n got %s", want, out)
		}
	}
}

// spec/10 §9.3：Chat 上游的停止原因经 IR 后，Responses 客户端与 Anthropic 客户端的语义一致。
func TestChatToResponsesStopReasons(t *testing.T) {
	mk := func(finish string) []byte {
		return []byte(`{"id":"chatcmpl-x","choices":[{"message":{"content":"hi"},"finish_reason":"` + finish + `"}]}`)
	}
	for finish, want := range map[string]string{
		"stop": "completed|", "tool_calls": "completed|", "": "completed|", "unknown_custom": "completed|",
		"length": "incomplete|max_output_tokens", "content_filter": "incomplete|content_filter",
	} {
		out, err := chatToResponsesBody(mk(finish), "m", fixedTime, seqIDs())
		if err != nil {
			t.Fatalf("%q: %v", finish, err)
		}
		var got struct {
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		reason := ""
		if got.IncompleteDetails != nil {
			reason = got.IncompleteDetails.Reason
		}
		if got.Status+"|"+reason != want {
			t.Errorf("finish %q → %s|%s, want %s", finish, got.Status, reason, want)
		}
	}
	_, err := chatToResponsesBody(mk("insufficient_system_resource"), "m", fixedTime, seqIDs())
	if ae, ok := apperr.As(err); !ok || ae.Code != apperr.CodeUpstreamError {
		t.Errorf("insufficient_system_resource: err = %v, want CodeUpstreamError", err)
	}

	first := chunkEv(`{"id":"chatcmpl-x","choices":[{"delta":{"content":"hi"}}]}`)
	upstream := &sliceStream{evs: []sse.Event{first, chunkEv(`{"choices":[{"delta":{},"finish_reason":"insufficient_system_resource"}]}`)}}
	resp, err := testResponsesChatTranslator().Response(responsesChatEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for last == nil {
		_, last = resp.Events.Next()
	}
	var ae *protocol.AttemptError
	if !errors.As(last, &ae) || ae.Outcome.Kind != protocol.OutcomeServerError {
		t.Errorf("stream insufficient_system_resource: final error = %v, want AttemptError server_error", last)
	}
}

// B9：Chat 上游流式的 refusal 发给 Anthropic 客户端时输出为 text，且 stop_reason 为 refusal，与非流式一致。
func TestChatToAnthropicStreamRefusal(t *testing.T) {
	upstream := &sliceStream{evs: []sse.Event{
		chunkEv(`{"id":"chatcmpl-r","choices":[{"delta":{"role":"assistant","refusal":"I cannot help."}}]}`),
		chunkEv(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
		chunkEv(`[DONE]`),
	}}
	out, err := testAnthropicTranslator().Response(anthropicEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
	if err != nil {
		t.Fatal(err)
	}
	got := drainChat(t, out.Events)
	checkAnthropicEvents(t, got)
	var all strings.Builder
	for _, ev := range got {
		all.Write(ev.Data)
	}
	for _, want := range []string{`"text":"I cannot help."`, `"stop_reason":"refusal"`} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("stream missing %s\n%s", want, all.String())
		}
	}
}
