package translate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

var geminiClientEnv = &protocol.Envelope{Protocol: protoGemini, Op: opGeminiCall, Model: "claude-for-gemini",
	Params: map[string]string{"method": "streamGenerateContent"}}

// geminiGoldenDirs 是 T48 新增的 5 个方向的响应黄金用例（spec/10 §10.5、§10.6）。
// 目录名 <上游>_to_<客户端>；input.* 是上游响应，expected.* 是客户端看到的输出。
var geminiGoldenDirs = []struct {
	dir      string
	from, to protocol.Protocol // 转换器方向：客户端 → 上游
	env      *protocol.Envelope
	encode   func([]sse.Event) []byte
	check    func(*testing.T, []sse.Event)
}{
	{"gemini_to_chat", protoOpenAIChat, protoGemini, chatEnv, encodeChatSSE, checkChatChunks},
	{"gemini_to_anthropic", protoAnthropic, protoGemini, anthropicEnv, encodeSSE, checkAnthropicEvents},
	{"gemini_to_responses", protoOpenAIResponses, protoGemini, responsesEnv, encodeSSE, checkSequence},
	{"chat_to_gemini", protoGemini, protoOpenAIChat, geminiClientEnv, encodeChatSSE, checkGeminiChunks},
	{"anthropic_to_gemini", protoGemini, protoAnthropic, geminiClientEnv, encodeChatSSE, checkGeminiChunks},
}

func testComposed(from, to protocol.Protocol) composed {
	return composed{from: from, to: to, now: func() time.Time { return fixedTime }, newID: seqIDs()}
}

func TestGeminiGolden(t *testing.T) {
	for _, g := range geminiGoldenDirs {
		inputs, _ := filepath.Glob(filepath.Join("testdata", g.dir, "*", "input.*"))
		if len(inputs) == 0 {
			t.Fatalf("%s: no golden cases", g.dir)
		}
		for _, in := range inputs {
			dir := filepath.Dir(in)
			t.Run(g.dir+"/"+filepath.Base(dir)+"/"+filepath.Base(in), func(t *testing.T) {
				raw, err := os.ReadFile(in)
				if err != nil {
					t.Fatal(err)
				}
				tr := testComposed(g.from, g.to)
				if filepath.Ext(in) == ".json" {
					out, err := tr.Response(g.env, &protocol.Response{Kind: protocol.ResponseBuffered, Status: 200, Body: raw})
					if err != nil {
						t.Fatalf("Response() error = %v", err)
					}
					var pretty bytes.Buffer
					if err := json.Indent(&pretty, out.Body, "", "  "); err != nil {
						t.Fatal(err)
					}
					pretty.WriteByte('\n')
					compareGolden(t, filepath.Join(dir, "expected.json"), pretty.Bytes())
					return
				}
				upstream := &sliceStream{evs: readSSE(t, raw)}
				out, err := tr.Response(g.env, &protocol.Response{Kind: protocol.ResponseEvents, Events: upstream})
				if err != nil {
					t.Fatalf("Response() error = %v", err)
				}
				got := drainChat(t, out.Events)
				g.check(t, got)
				if err := out.Events.Close(); err != nil || !upstream.closed {
					t.Errorf("Close() did not close upstream (err=%v)", err)
				}
				compareGolden(t, filepath.Join(dir, "expected.sse"), g.encode(got))
			})
		}
	}
}

// checkGeminiChunks 校验 Gemini 客户端流的结构约束（spec/10 §10.6）：每个 chunk 都是合法的
// GenerateContentResponse（恰好 1 个候选、role 为 model）；只有最后一个带 finishReason 与 usageMetadata；
// functionCall.args 是对象；responseId 与 modelVersion 全程相同；没有事件名，也没有 [DONE]。
func checkGeminiChunks(t *testing.T, evs []sse.Event) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("empty stream")
	}
	var id, model string
	for i, ev := range evs {
		var c struct {
			Candidates []struct {
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						FunctionCall *struct {
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct{} `json:"usageMetadata"`
			ModelVersion  string    `json:"modelVersion"`
			ResponseID    string    `json:"responseId"`
		}
		if ev.Name != "" {
			t.Errorf("chunk %d: event name %q, want none", i, ev.Name)
		}
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			t.Fatalf("chunk %d is not JSON (%v): %s", i, err, ev.Data)
		}
		if i == 0 {
			id, model = c.ResponseID, c.ModelVersion
		}
		if c.ResponseID == "" || c.ResponseID != id || c.ModelVersion != model {
			t.Errorf("chunk %d: responseId/modelVersion = %q/%q, want %q/%q", i, c.ResponseID, c.ModelVersion, id, model)
		}
		if len(c.Candidates) != 1 || c.Candidates[0].Content.Role != "model" {
			t.Fatalf("chunk %d: want exactly one model candidate: %s", i, ev.Data)
		}
		last := i == len(evs)-1
		if (c.Candidates[0].FinishReason != "") != last || (c.UsageMetadata != nil) != last {
			t.Errorf("chunk %d: finishReason=%q usage=%v; only the last chunk may carry them", i, c.Candidates[0].FinishReason, c.UsageMetadata != nil)
		}
		for _, p := range c.Candidates[0].Content.Parts {
			if p.FunctionCall != nil && (len(p.FunctionCall.Args) == 0 || p.FunctionCall.Args[0] != '{') {
				t.Errorf("chunk %d: functionCall.args is not an object: %s", i, p.FunctionCall.Args)
			}
		}
	}
}
