package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

var fixedTime = time.Unix(1_700_000_000, 0)

// seqIDs 返回按前缀计数的确定性 ID 生成器：resp_1、msg_1、fc_1、fc_2 ...
func seqIDs() func(string) string {
	n := map[string]int{}
	return func(prefix string) string {
		n[prefix]++
		return fmt.Sprintf("%s%d", prefix, n[prefix])
	}
}

func testTranslator() ResponsesToAnthropic {
	return ResponsesToAnthropic{Now: func() time.Time { return fixedTime }, NewID: seqIDs()}
}

// sliceStream 是测试用的 EventStream。
type sliceStream struct {
	evs    []sse.Event
	end    error // 事件读完后返回的错误；nil = io.EOF
	closed bool
}

func (s *sliceStream) Next() (sse.Event, error) {
	if len(s.evs) == 0 {
		if s.end != nil {
			return sse.Event{}, s.end
		}
		return sse.Event{}, io.EOF
	}
	ev := s.evs[0]
	s.evs = s.evs[1:]
	return ev, nil
}

func (s *sliceStream) Close() error { s.closed = true; return nil }

func readSSE(t *testing.T, b []byte) []sse.Event {
	t.Helper()
	r := sse.NewReader(bytes.NewReader(b))
	var evs []sse.Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return evs
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
}

// encodeSSE 以「event 行 + 缩进的 JSON」输出事件，便于人工核对黄金文件。
func encodeSSE(evs []sse.Event) []byte {
	var b bytes.Buffer
	for _, ev := range evs {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, ev.Data, "", "  "); err != nil {
			pretty.Write(ev.Data)
		}
		fmt.Fprintf(&b, "event: %s\n", ev.Name)
		for _, line := range strings.Split(pretty.String(), "\n") {
			fmt.Fprintf(&b, "data: %s\n", line)
		}
		b.WriteString("\n")
	}
	return b.Bytes()
}

func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

var responsesEnv = &protocol.Envelope{Protocol: protoOpenAIResponses, Op: opResponsesCreate, Model: "claude-for-codex"}

// spec/10 §8.3 非流式 + §8.4：testdata/anthropic_to_responses/<用例>/{input.json, expected.json}。
func TestAnthropicToResponsesBufferedGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "anthropic_to_responses", "*", "input.json"))
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
			out, err := testTranslator().Response(responsesEnv, &protocol.Response{Kind: protocol.ResponseBuffered, Body: body})
			if err != nil {
				t.Fatalf("Response() error = %v", err)
			}
			if ct := out.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
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

// spec/10 §8.3 流式 + §8.4：testdata/anthropic_to_responses/<用例>/{input.sse, expected.sse}。
func TestAnthropicToResponsesStreamGolden(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "anthropic_to_responses", "*", "input.sse"))
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
			out, err := testTranslator().Response(responsesEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
			if err != nil {
				t.Fatalf("Response() error = %v", err)
			}
			var got []sse.Event
			for {
				ev, err := out.Events.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next() error = %v", err)
				}
				got = append(got, ev)
			}
			checkSequence(t, got)
			if err := out.Events.Close(); err != nil || !upstream.closed {
				t.Errorf("Close() did not close upstream (err=%v)", err)
			}
			compareGolden(t, filepath.Join(dir, "expected.sse"), encodeSSE(got))
		})
	}
}

// checkSequence 校验 §8.3 的通用约束：event 名与 data.type 相同，sequence_number 从 0 递增。
func checkSequence(t *testing.T, evs []sse.Event) {
	t.Helper()
	for i, ev := range evs {
		var d struct {
			Type string `json:"type"`
			Seq  int    `json:"sequence_number"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if d.Type != ev.Name || d.Seq != i {
			t.Errorf("event %d: name=%q type=%q seq=%d", i, ev.Name, d.Type, d.Seq)
		}
	}
}

// 上游 error 事件、上游流在 message_stop 之前结束：返回 *AttemptError，内核据此写入站 StreamError。
func TestAnthropicToResponsesStreamErrors(t *testing.T) {
	start := sse.Event{Name: "message_start", Data: []byte(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)}
	tests := []struct {
		name     string
		upstream *sliceStream
		wantKind protocol.OutcomeKind
	}{
		{"error event", &sliceStream{evs: []sse.Event{start,
			{Name: "error", Data: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)}}},
			protocol.OutcomeOverloaded},
		{"truncated", &sliceStream{evs: []sse.Event{start}}, protocol.OutcomeNetworkError},
		{"upstream read error", &sliceStream{evs: []sse.Event{start},
			end: &protocol.AttemptError{Outcome: protocol.Outcome{Kind: protocol.OutcomeTimeout}}}, protocol.OutcomeTimeout},
		{"bad json", &sliceStream{evs: []sse.Event{{Name: "message_start", Data: []byte(`{`)}}}, protocol.OutcomeServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := testTranslator().Response(responsesEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: tt.upstream})
			if err != nil {
				t.Fatal(err)
			}
			var last error
			for range 20 {
				if _, last = out.Events.Next(); last != nil {
					break
				}
			}
			var ae *protocol.AttemptError
			if !errors.As(last, &ae) || ae.Outcome.Kind != tt.wantKind {
				t.Fatalf("final error = %v, want AttemptError kind %v", last, tt.wantKind)
			}
		})
	}
}

// Request：Responses → IR → Anthropic；RX-01 等拒绝规则透传为 400。
func TestResponsesToAnthropicRequest(t *testing.T) {
	tr := ResponsesToAnthropic{}
	if tr.From() != protoOpenAIResponses || tr.To() != protoAnthropic {
		t.Fatalf("From/To = %s/%s", tr.From(), tr.To())
	}
	env := &protocol.Envelope{Op: opResponsesCreate, Model: "claude-for-codex", Stream: true,
		Body: []byte(`{"model":"claude-for-codex","instructions":"Be brief.","input":"Hi","stream":true,"max_output_tokens":100}`)}
	call, err := tr.Request(env, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if call.Op != opAnthropicCreate || call.Model != "claude-sonnet-4-5" || !call.Stream {
		t.Errorf("call = %+v", call)
	}
	var body struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		System    []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "claude-sonnet-4-5" || body.MaxTokens != 100 || !body.Stream || len(body.System) != 1 || body.System[0].Text != "Be brief." {
		t.Errorf("anthropic body = %s", call.Body)
	}

	for name, env := range map[string]*protocol.Envelope{
		"previous_response_id": {Op: opResponsesCreate, Model: "m", Body: []byte(`{"model":"m","input":"hi","previous_response_id":"resp_1"}`)},
		"get by id (RX-04)":    {Op: "responses.get", Model: "m"},
	} {
		if _, err := tr.Request(env, "up"); !isInvalid(err) {
			t.Errorf("%s: error = %v, want invalid_request", name, err)
		}
	}
}

func isInvalid(err error) bool {
	ae, ok := apperr.As(err)
	return ok && ae.Code == apperr.CodeInvalidRequest
}

// RX-05：网关签发的 ID 为 前缀 + 24 位小写 base32，且每次不同。
func TestNewGatewayID(t *testing.T) {
	a, b := newGatewayID("resp_"), newGatewayID("resp_")
	if a == b || len(a) != len("resp_")+24 || strings.ToLower(a) != a {
		t.Fatalf("ids %q %q", a, b)
	}
	for _, c := range strings.TrimPrefix(a, "resp_") {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz234567", c) {
			t.Fatalf("id %q has non-base32 char %q", a, c)
		}
	}
}

// 字节流原样返回。
func TestResponsesToAnthropicRawPassthrough(t *testing.T) {
	in := &protocol.Response{Kind: protocol.ResponseRaw}
	out, err := ResponsesToAnthropic{}.Response(responsesEnv, in)
	if err != nil || out != in {
		t.Fatalf("Response(raw) = %v, %v", out, err)
	}
}

// spec/10 §8.3：max_tokens → max_output_tokens，refusal → content_filter，pause_turn 等其他值 → completed。
func TestStatusFor(t *testing.T) {
	tests := []struct {
		stopReason string
		wantStatus string
		wantReason string
	}{
		{"max_tokens", "incomplete", "max_output_tokens"},
		{"refusal", "incomplete", "content_filter"},
		{"pause_turn", "completed", ""},
		{"end_turn", "completed", ""},
		{"stop_sequence", "completed", ""},
		{"tool_use", "completed", ""},
		{"", "completed", ""},
		{"unknown", "completed", ""},
	}
	for _, tt := range tests {
		status, inc := statusFor(tt.stopReason)
		if status != tt.wantStatus {
			t.Errorf("statusFor(%q) status = %q, want %q", tt.stopReason, status, tt.wantStatus)
		}
		if tt.wantReason == "" {
			if inc != nil {
				t.Errorf("statusFor(%q) inc = %+v, want nil", tt.stopReason, inc)
			}
		} else {
			if inc == nil || inc.Reason != tt.wantReason {
				t.Errorf("statusFor(%q) inc = %+v, want reason %q", tt.stopReason, inc, tt.wantReason)
			}
		}
	}
}

func TestAnthropicToResponsesRefusal(t *testing.T) {
	for _, stop := range []string{"refusal"} {
		body := []byte(fmt.Sprintf(`{"id":"msg_1","content":[{"type":"text","text":"Blocked"}],"stop_reason":%q,"usage":{"input_tokens":5,"output_tokens":2}}`, stop))
		out, err := anthropicToResponsesBody(body, "m", fixedTime, seqIDs())
		if err != nil {
			t.Fatalf("stop_reason=%s error = %v", stop, err)
		}
		var resp respObject
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != "incomplete" {
			t.Errorf("status = %q, want incomplete", resp.Status)
		}
		if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "content_filter" {
			t.Errorf("incomplete_details = %+v, want content_filter", resp.IncompleteDetails)
		}
	}
}

func TestAnthropicToResponsesStreamRefusal(t *testing.T) {
	upstream := &sliceStream{evs: []sse.Event{
		{Name: "message_start", Data: []byte(`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`)},
		{Name: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Blocked"}}`)},
		{Name: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":2}}`)},
		{Name: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	}}
	out, err := testTranslator().Response(responsesEnv, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
	if err != nil {
		t.Fatal(err)
	}
	got := drainChat(t, out.Events)
	last := got[len(got)-1]
	if last.Name != "response.incomplete" {
		t.Fatalf("last event = %q, want response.incomplete", last.Name)
	}
	var data struct {
		Response respObject `json:"response"`
	}
	if err := json.Unmarshal(last.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Response.Status != "incomplete" || data.Response.IncompleteDetails == nil || data.Response.IncompleteDetails.Reason != "content_filter" {
		t.Errorf("unexpected response: %+v", data.Response)
	}
}
