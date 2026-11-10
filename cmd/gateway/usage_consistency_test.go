package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/gemini"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/translate"
)

// 本文件实现 spec/10 §9.4 REG-04：对每个已注册的转译方向，同一份上游响应
// 交给出站适配器的 Meter 算出的计费用量，必须与经转换器输出给客户端的 usage 逐计量单位一致。
//
// 黄金输入取 internal/translate/testdata/<上游>_to_<客户端>/<用例>/input.{json,sse}。

// usageProto 描述一个协议在本测试中的角色信息。
type usageProto struct {
	dir    string          // testdata 目录名中的协议简称
	op     string          // 该协议的操作名（作客户端时的 Envelope.Op，作上游时的 Call.Op）
	egress protocol.Egress // 作为上游时的出站适配器；nil 表示不能作上游
	// foldsCacheWrite 为 true 表示该客户端协议没有「缓存写入」字段：缓存写入 token 计入它的输入数，
	// 因此比较时把计费侧的 cache_write_tokens 并入 input_tokens（Chat、Responses 的 usage 都是这种形状）。
	foldsCacheWrite bool
	// clientUsage 从该协议客户端看到的响应体或流事件中解析出计量单位；found=false 表示输出里没有 usage。
	clientUsage func(body []byte, events [][]byte) (u pricing.Usage, found bool, err error)
}

var usageProtos = map[protocol.Protocol]usageProto{
	"openai_chat":      {dir: "chat", op: "chat.completions", egress: openai.Egress{}, foldsCacheWrite: true, clientUsage: chatClientUsage},
	"anthropic":        {dir: "anthropic", op: "messages", egress: anthropic.Egress{}, clientUsage: anthropicClientUsage},
	"openai_responses": {dir: "responses", op: "responses.create", foldsCacheWrite: true, clientUsage: responsesClientUsage},
	"gemini":           {dir: "gemini", op: "models.call", egress: gemini.Egress{}, foldsCacheWrite: true, clientUsage: geminiClientUsage},
}

func TestUsageConsistency(t *testing.T) {
	reg := protocol.NewRegistry()
	if err := translate.Register(reg); err != nil {
		t.Fatalf("translate.Register() error = %v", err)
	}
	names := make([]string, 0, len(usageProtos))
	for p := range usageProtos {
		names = append(names, string(p))
	}
	sort.Strings(names)

	directions := 0
	for _, from := range names {
		for _, to := range names {
			tr, ok := reg.Translator(protocol.Protocol(from), protocol.Protocol(to))
			if !ok {
				continue
			}
			directions++
			client, up := usageProtos[protocol.Protocol(from)], usageProtos[protocol.Protocol(to)]
			if up.egress == nil {
				t.Fatalf("direction %s -> %s: upstream %s has no egress in this test; add it to usageProtos", from, to, to)
			}
			dir := filepath.Join("..", "..", "internal", "translate", "testdata", up.dir+"_to_"+client.dir)
			cases, err := filepath.Glob(filepath.Join(dir, "*", "input.*"))
			if err != nil || len(cases) == 0 {
				t.Fatalf("direction %s -> %s: no golden inputs under %s (err=%v); every direction must be covered", from, to, dir, err)
			}
			checked := 0
			for _, in := range cases {
				name := from + "_from_" + to + "/" + filepath.Base(filepath.Dir(in)) + "/" + filepath.Base(in)
				t.Run(name, func(t *testing.T) {
					if checkUsageCase(t, tr, client, up, protocol.Protocol(from), in) {
						checked++
					}
				})
			}
			if checked == 0 {
				t.Errorf("direction %s -> %s: no golden case carries upstream usage; add one", from, to)
			}
		}
	}
	if directions == 0 {
		t.Fatal("no translators registered")
	}
}

// checkUsageCase 跑一个黄金用例；返回 true 表示该用例带上游用量并完成了比较。
func checkUsageCase(t *testing.T, tr protocol.Translator, client, up usageProto, clientProto protocol.Protocol, inputPath string) bool {
	t.Helper()
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read %s: %v", inputPath, err)
	}
	stream := filepath.Ext(inputPath) == ".sse"
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data) // 假上游；写失败会在读取方表现为截断
	}))
	defer srv.Close()

	clientBody := []byte(`{}`)
	if stream {
		clientBody = []byte(`{"stream":true,"stream_options":{"include_usage":true}}`)
	}
	env := &protocol.Envelope{Protocol: clientProto, Op: client.op, Model: "m", Stream: stream, Body: clientBody}
	call := &protocol.Call{Op: up.op, Model: "m", Stream: stream, Body: []byte(`{}`)}
	upstream := protocol.Upstream{BaseURL: srv.URL, Secret: []byte("test-secret"), Client: srv.Client(), MaxBufferedBytes: 8 << 20}

	resp, err := up.egress.Do(context.Background(), upstream, call)
	if err != nil {
		t.Fatalf("egress Do() error = %v", err)
	}
	out, err := tr.Response(env, resp)
	if err != nil {
		t.Logf("translator rejected this response (%v); not a usage case", err)
		return false
	}
	var body []byte
	var events [][]byte
	switch out.Kind {
	case protocol.ResponseBuffered:
		body = out.Body
	case protocol.ResponseEvents:
		defer func() { _ = out.Events.Close() }() // 清理；读取错误已在循环中处理
		for {
			ev, err := out.Events.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Logf("translated stream failed (%v); not a usage case", err)
				return false
			}
			events = append(events, ev.Data)
		}
	default:
		t.Fatalf("Response() kind = %v, want buffered or events", out.Kind)
	}

	meterUsage, src := resp.Meter.Usage()
	clientUsage, found, err := client.clientUsage(body, events)
	if err != nil {
		t.Fatalf("parse client usage: %v", err)
	}
	if src != protocol.UsageUpstream {
		if found && nonZero(clientUsage) {
			t.Errorf("meter has no upstream usage (source %v) but client sees %v", src, clientUsage)
		}
		return false
	}
	if !found {
		t.Errorf("meter usage %v (upstream) but client output carries no usage", meterUsage)
		return true
	}

	billed := pricing.Usage{}
	for u, n := range meterUsage {
		billed[u] = n
	}
	if client.foldsCacheWrite {
		// 客户端协议没有缓存写入字段：缓存写入计入客户端看到的输入数（tokenUsageToChat / tokenUsageToResponses）。
		billed[pricing.UnitInputTokens] += billed[pricing.UnitCacheWriteTokens]
		delete(billed, pricing.UnitCacheWriteTokens)
	}
	units := map[string]bool{}
	for u := range billed {
		units[u] = true
	}
	for u := range clientUsage {
		units[u] = true
	}
	for u := range units {
		if billed[u] != clientUsage[u] {
			t.Errorf("unit %s: billed (meter) = %d, client-visible = %d; meter=%v client=%v", u, billed[u], clientUsage[u], meterUsage, clientUsage)
		}
	}
	return true
}

func nonZero(u pricing.Usage) bool {
	for _, n := range u {
		if n != 0 {
			return true
		}
	}
	return false
}

// chatClientUsage：非流式取顶层 usage；流式取最后一个带 usage 的 chunk。
// input_tokens = prompt_tokens − cached_tokens（与 OpenAI 出站适配器的计费口径相同）。
func chatClientUsage(body []byte, events [][]byte) (pricing.Usage, bool, error) {
	type chatUsage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	var last *chatUsage
	docs := events
	if body != nil {
		docs = [][]byte{body}
	}
	for _, d := range docs {
		if string(d) == "[DONE]" { // Chat 流的结束标记，不是 JSON
			continue
		}
		var v struct {
			Usage *chatUsage `json:"usage"`
		}
		if err := json.Unmarshal(d, &v); err != nil {
			return nil, false, err
		}
		if v.Usage != nil {
			last = v.Usage
		}
	}
	if last == nil {
		return nil, false, nil
	}
	cached := last.PromptTokensDetails.CachedTokens
	return pricing.Usage{
		pricing.UnitInputTokens:     max(last.PromptTokens-cached, 0),
		pricing.UnitCacheReadTokens: cached,
		pricing.UnitOutputTokens:    last.CompletionTokens,
	}, true, nil
}

// anthropicClientUsage：非流式取顶层 usage；流式取最后一个 message_delta 的 usage（累计值）。
func anthropicClientUsage(body []byte, events [][]byte) (pricing.Usage, bool, error) {
	type anthUsage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	}
	var last *anthUsage
	if body != nil {
		var v struct {
			Usage *anthUsage `json:"usage"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, false, err
		}
		last = v.Usage
	}
	for _, d := range events {
		var v struct {
			Type  string     `json:"type"`
			Usage *anthUsage `json:"usage"`
		}
		if err := json.Unmarshal(d, &v); err != nil {
			return nil, false, err
		}
		if v.Type == "message_delta" && v.Usage != nil {
			last = v.Usage
		}
	}
	if last == nil {
		return nil, false, nil
	}
	return pricing.Usage{
		pricing.UnitInputTokens:      last.InputTokens,
		pricing.UnitOutputTokens:     last.OutputTokens,
		pricing.UnitCacheReadTokens:  last.CacheReadInputTokens,
		pricing.UnitCacheWriteTokens: last.CacheCreationInputTokens,
	}, true, nil
}

// responsesClientUsage：非流式取顶层 usage；流式取 response.completed / response.incomplete 中的 response.usage。
// input_tokens 含缓存，换算为 input_tokens − cached_tokens。
func responsesClientUsage(body []byte, events [][]byte) (pricing.Usage, bool, error) {
	type respUsage struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	var last *respUsage
	if body != nil {
		var v struct {
			Usage *respUsage `json:"usage"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, false, err
		}
		last = v.Usage
	}
	for _, d := range events {
		var v struct {
			Response struct {
				Usage *respUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(d, &v); err != nil {
			return nil, false, err
		}
		if v.Response.Usage != nil {
			last = v.Response.Usage
		}
	}
	if last == nil {
		return nil, false, nil
	}
	cached := last.InputTokensDetails.CachedTokens
	return pricing.Usage{
		pricing.UnitInputTokens:     max(last.InputTokens-cached, 0),
		pricing.UnitCacheReadTokens: cached,
		pricing.UnitOutputTokens:    last.OutputTokens,
	}, true, nil
}

// geminiClientUsage：取最后一个带 usageMetadata 的响应体或 chunk。
// promptTokenCount 含缓存，换算为 input_tokens = promptTokenCount − cachedContentTokenCount + toolUsePromptTokenCount；
// output_tokens = candidatesTokenCount + thoughtsTokenCount（与 USE-G-02 相同）。
func geminiClientUsage(body []byte, events [][]byte) (pricing.Usage, bool, error) {
	type gemUsage struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
	}
	var last *gemUsage
	docs := events
	if body != nil {
		docs = [][]byte{body}
	}
	for _, d := range docs {
		var v struct {
			UsageMetadata *gemUsage `json:"usageMetadata"`
		}
		if err := json.Unmarshal(d, &v); err != nil {
			return nil, false, err
		}
		if v.UsageMetadata != nil {
			last = v.UsageMetadata
		}
	}
	if last == nil {
		return nil, false, nil
	}
	cached := last.CachedContentTokenCount
	return pricing.Usage{
		pricing.UnitInputTokens:     max(last.PromptTokenCount-cached, 0) + last.ToolUsePromptTokenCount,
		pricing.UnitCacheReadTokens: cached,
		pricing.UnitOutputTokens:    last.CandidatesTokenCount + last.ThoughtsTokenCount,
	}, true, nil
}
