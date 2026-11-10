package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// fakeGeminiUpstream 模拟 Gemini 上游：记录收到的请求，返回带 finishReason 的完整响应（spec/10 §10.5 要求流以 finishReason 结束）。
type fakeGeminiUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recordedReq
}

type recordedReq struct {
	path, query, apiKey string
	body                []byte
}

func newFakeGeminiUpstream(t *testing.T) *fakeGeminiUpstream {
	t.Helper()
	f := &fakeGeminiUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 测试桩：读失败时下面的断言会失败
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedReq{path: r.URL.Path, query: r.URL.RawQuery, apiKey: r.Header.Get("x-goog-api-key"), body: body})
		f.mu.Unlock()
		switch r.URL.Path {
		case "/v1beta/models/gemini-3-pro:generateContent":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Rome"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":6,"cachedContentTokenCount":2},"responseId":"g1"}`)
		case "/v1beta/models/gemini-3-pro:streamGenerateContent":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Sunny \"}]}}],\"responseId\":\"g2\"}\n\n"+
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"in Rome\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":20,\"candidatesTokenCount\":3},\"responseId\":\"g2\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGeminiUpstream) last(t *testing.T) recordedReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("gemini upstream received no request")
	}
	return f.reqs[len(f.reqs)-1]
}

// TestE2EGeminiTranslate 走完整网关：Anthropic 客户端（Claude Code）→ Gemini 上游的多轮工具调用，
// 以及 Gemini 客户端 → Anthropic 上游的流式对话（spec/10 §10，T48）。
func TestE2EGeminiTranslate(t *testing.T) {
	anthUp := fakeUpstream(t)
	gemUp := newFakeGeminiUpstream(t)
	g := startGateway(t, anthUp.URL, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "gem", Protocol: "gemini", BaseURL: gemUp.srv.URL})
		c.Models = append(c.Models, config.ModelConfig{
			Name: "gem-for-claude", Pool: "gem", UpstreamModel: "gemini-3-pro",
			Price:   map[string]string{"input_tokens": "1.25/1M", "output_tokens": "10/1M", "cache_read_tokens": "0.125/1M"},
			Reserve: map[string]int64{"output_tokens": 1024},
		})
		c.Normalize()
	})
	adminURL := "http://" + g.admin + "/admin/api"
	dataURL := "http://" + g.data
	var created struct {
		Token string `json:"token"`
	}
	if code := doJSON(t, "POST", adminURL+"/clients", g.adminToken,
		map[string]any{"name": "t48", "role": "user", "quota_nano_usd": int64(1e12)}, &created); code != http.StatusCreated {
		t.Fatalf("create client: %d", code)
	}
	for _, k := range []map[string]any{
		{"pool": "gem", "name": "gem-k1", "secret": "gemini-upstream-secret"},
		{"pool": "anth", "name": "anth-k1", "secret": "anthropic-upstream-secret"},
	} {
		if code := doJSON(t, "POST", adminURL+"/keys", g.adminToken, k, nil); code != http.StatusCreated {
			t.Fatalf("create key %v: %d", k["name"], code)
		}
	}

	post := func(t *testing.T, url, header, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		req.Header.Set(header, created.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("anthropic client to gemini upstream, tool round trip", func(t *testing.T) {
		body := `{"model":"gem-for-claude","max_tokens":256,
			"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
			"messages":[
				{"role":"user","content":"Weather in Paris?"},
				{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}
			]}`
		resp := post(t, dataURL+"/v1/messages", "x-api-key", body)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
		}
		up := gemUp.last(t)
		if up.path != "/v1beta/models/gemini-3-pro:generateContent" || up.apiKey != "gemini-upstream-secret" {
			t.Fatalf("upstream request path=%q key-set=%v", up.path, up.apiKey != "")
		}
		for _, want := range []string{
			`"functionResponse":{"name":"get_weather","response":{"output":"sunny"}}`, // GID-02
			`"thoughtSignature":"skip_thought_signature_validator"`,                   // GSIG-02（gemini-3）
			`"parametersJsonSchema":{"type":"object"`,                                 // §10.4
		} {
			if !bytes.Contains(up.body, []byte(want)) {
				t.Errorf("upstream body missing %s:\n%s", want, up.body)
			}
		}
		if bytes.Contains(up.body, []byte("gem-for-claude")) || bytes.Contains(up.body, []byte("gemini-3-pro")) {
			t.Errorf("upstream body must not contain a model name: %s", up.body)
		}
		var out struct {
			Content []struct {
				Type, ID, Name string
				Input          json.RawMessage
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens          int64 `json:"input_tokens"`
				CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
				OutputTokens         int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if len(out.Content) != 1 || out.Content[0].Type != "tool_use" || !strings.HasPrefix(out.Content[0].ID, "call_") ||
			out.StopReason != "tool_use" || out.Usage.InputTokens != 10 || out.Usage.CacheReadInputTokens != 2 || out.Usage.OutputTokens != 6 {
			t.Fatalf("response = %s", raw)
		}
	})

	t.Run("anthropic client to gemini upstream, stream", func(t *testing.T) {
		resp := post(t, dataURL+"/v1/messages", "x-api-key", `{"model":"gem-for-claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Weather?"}]}`)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
		}
		up := gemUp.last(t)
		if up.path != "/v1beta/models/gemini-3-pro:streamGenerateContent" || up.query != "alt=sse" {
			t.Fatalf("upstream path=%q query=%q", up.path, up.query)
		}
		for _, want := range []string{"event: message_start", `"text":"Sunny "`, `"text":"in Rome"`, `"stop_reason":"end_turn"`, "event: message_stop"} {
			if !bytes.Contains(raw, []byte(want)) {
				t.Errorf("stream missing %s:\n%s", want, raw)
			}
		}
	})

	t.Run("gemini client to anthropic upstream, stream", func(t *testing.T) {
		resp := post(t, dataURL+"/v1beta/models/claude-test:streamGenerateContent?alt=sse", "x-goog-api-key", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
		}
		var text, finish string
		var prompt int64
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			if data == "[DONE]" {
				t.Fatal("gemini stream must not contain [DONE]")
			}
			var c struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"content"`
					FinishReason string `json:"finishReason"`
				} `json:"candidates"`
				UsageMetadata *struct {
					PromptTokenCount int64 `json:"promptTokenCount"`
				} `json:"usageMetadata"`
			}
			if err := json.Unmarshal([]byte(data), &c); err != nil {
				t.Fatalf("chunk %q: %v", data, err)
			}
			for _, p := range c.Candidates[0].Content.Parts {
				text += p.Text
			}
			if c.Candidates[0].FinishReason != "" {
				finish = c.Candidates[0].FinishReason
			}
			if c.UsageMetadata != nil {
				prompt = c.UsageMetadata.PromptTokenCount
			}
		}
		if text != "hello" || finish != "STOP" || prompt != 5 {
			t.Fatalf("text=%q finish=%q prompt=%d", text, finish, prompt)
		}
	})

	t.Run("gemini client countTokens on anthropic upstream is 400", func(t *testing.T) {
		resp := post(t, dataURL+"/v1beta/models/claude-test:countTokens", "x-goog-api-key", `{"contents":[{"parts":[{"text":"hi"}]}]}`)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(raw, []byte("countTokens is not supported")) || !bytes.Contains(raw, []byte(`"status":"INVALID_ARGUMENT"`)) {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
		}
	})
}
