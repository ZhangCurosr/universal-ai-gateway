package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// 假上游（T77 §3）：openai（/v1/chat/completions）、anthropic（/v1/messages、/v1/messages/count_tokens）、
// responses（/v1/responses）。回答缺省为 pong；最后一条用户消息以 echo: 开头时回显其余部分；
// 带工具定义且最后一条消息（用户消息）包含 call:<工具名> 时返回一次工具调用（参数 {}）。

// fakeProtocols 是 --fake 支持的名字 → 配置中对应池的 protocol。
var fakeProtocols = map[string]string{
	"openai":    "openai_chat",
	"anthropic": "anthropic",
	"responses": "openai_responses",
}

const maxFakeBody = 32 << 20

// fakeReq 是从请求中提取出的、决定回答所需的信息。
type fakeReq struct {
	Model         string
	Stream        bool
	IncludeUsage  bool
	System        bool     // anthropic 顶层 system
	Roles         []string // 消息（或 responses 输入项）的角色序列
	Tools         []string
	lastRole      string
	lastText      string
	lastUserParts []string // 最后一条用户消息的各文本块
}

// fakeLog 在 <目录>/fake.log 每个请求记一行（不记内容）。
type fakeLog struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *fakeLog) record(path string, r fakeReq) {
	if l == nil || l.w == nil {
		return
	}
	roles := r.Roles
	if roles == nil {
		roles = []string{}
	}
	b, err := json.Marshal(map[string]any{"path": path, "model": r.Model, "stream": r.Stream, "system": r.System, "roles": roles, "tools": len(r.Tools)})
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "devgw: write fake.log:", err)
	}
}

// answer 决定回答：返回文本，或要调用的工具名（非空时 text 为空）。
func answer(r fakeReq) (text, tool string) {
	if r.lastRole == "user" {
		if i := strings.Index(r.lastText, "call:"); i >= 0 {
			name := r.lastText[i+len("call:"):]
			if j := strings.IndexAny(name, " \t\r\n,.;"); j >= 0 {
				name = name[:j]
			}
			for _, t := range r.Tools {
				if t == name && name != "" {
					return "", name
				}
			}
		}
	}
	// 按文本块判断：Claude Code 等客户端会在用户消息前面插入提醒文本块。
	for i := len(r.lastUserParts) - 1; i >= 0; i-- {
		if rest, ok := strings.CutPrefix(strings.TrimLeft(r.lastUserParts[i], " \t\r\n"), "echo:"); ok {
			return rest, ""
		}
	}
	return "pong", ""
}

func outputTokens(text string) int { return len([]rune(text))/4 + 1 }

const fakeInputTokens = 12

// contentParts 取 content（字符串或 [{type, text}] 数组）中的各文本块。
func contentParts(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			out = append(out, p.Text)
		}
	}
	return out
}

type roleMsg struct {
	Role    string          `json:"role"`
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

func (r *fakeReq) addMessages(msgs []roleMsg) {
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = m.Type
		}
		r.Roles = append(r.Roles, role)
		parts := contentParts(m.Content)
		r.lastRole, r.lastText = role, strings.Join(parts, "\n")
		if role == "user" {
			r.lastUserParts = parts
		}
	}
}

func decodeBody(w http.ResponseWriter, req *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxFakeBody))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		http.Error(w, `{"error":{"message":"invalid JSON body"}}`, http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSONBody(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	if _, err := w.Write(b); err != nil {
		return // 客户端断开
	}
}

// sse 写事件流；event 为空时只写 data 行（OpenAI 格式）。
type sse struct {
	w http.ResponseWriter
	f http.Flusher
}

func startSSE(w http.ResponseWriter) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher) // httptest 与 net/http 的 ResponseWriter 都实现了 Flusher
	return &sse{w: w, f: f}
}

func (s *sse) send(event string, v any) {
	var b []byte
	if raw, ok := v.(string); ok {
		b = []byte(raw)
	} else {
		var err error
		if b, err = json.Marshal(v); err != nil {
			return
		}
	}
	var out strings.Builder
	if event != "" {
		out.WriteString("event: " + event + "\n")
	}
	out.WriteString("data: " + string(b) + "\n\n")
	if _, err := io.WriteString(s.w, out.String()); err != nil {
		return
	}
	if s.f != nil {
		s.f.Flush()
	}
}

// newFakeHandler 返回某个协议的假上游。
func newFakeHandler(name string, log *fakeLog) (http.Handler, error) {
	mux := http.NewServeMux()
	switch name {
	case "openai":
		mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, req *http.Request) { fakeOpenAI(w, req, log) })
	case "anthropic":
		mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, req *http.Request) { fakeAnthropic(w, req, log) })
		mux.HandleFunc("POST /v1/messages/count_tokens", func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				Model    string    `json:"model"`
				Messages []roleMsg `json:"messages"`
			}
			if !decodeBody(w, req, &body) {
				return
			}
			fr := fakeReq{Model: body.Model}
			fr.addMessages(body.Messages)
			log.record(req.URL.Path, fr)
			writeJSONBody(w, map[string]any{"input_tokens": fakeInputTokens})
		})
	case "responses":
		mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, req *http.Request) { fakeResponses(w, req, log) })
	default:
		return nil, fmt.Errorf("unknown fake upstream %q (want openai, anthropic or responses)", name)
	}
	return mux, nil
}

// ---------------------------------------------------------------- openai

func fakeOpenAI(w http.ResponseWriter, req *http.Request, log *fakeLog) {
	var body struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []roleMsg `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	fr := fakeReq{Model: body.Model, Stream: body.Stream, IncludeUsage: body.StreamOptions != nil && body.StreamOptions.IncludeUsage}
	fr.addMessages(body.Messages)
	for _, t := range body.Tools {
		fr.Tools = append(fr.Tools, t.Function.Name)
	}
	log.record(req.URL.Path, fr)
	text, tool := answer(fr)
	created := time.Now().Unix()
	usage := map[string]int{"prompt_tokens": fakeInputTokens, "completion_tokens": outputTokens(text + tool), "total_tokens": fakeInputTokens + outputTokens(text+tool)}
	finish := "stop"
	if tool != "" {
		finish = "tool_calls"
	}
	if !fr.Stream {
		msg := map[string]any{"role": "assistant", "content": text}
		if tool != "" {
			msg["content"] = nil
			msg["tool_calls"] = []any{map[string]any{"id": "call_fake", "type": "function", "function": map[string]any{"name": tool, "arguments": "{}"}}}
		}
		writeJSONBody(w, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "created": created, "model": fr.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
		return
	}
	s := startSSE(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": created, "model": fr.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	if tool != "" {
		s.send("", chunk(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"index": 0, "id": "call_fake", "type": "function", "function": map[string]any{"name": tool, "arguments": ""}}}}, nil))
		s.send("", chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "{}"}}}}, nil))
	} else {
		s.send("", chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		s.send("", chunk(map[string]any{"content": text}, nil))
	}
	s.send("", chunk(map[string]any{}, finish))
	if fr.IncludeUsage {
		s.send("", map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": created, "model": fr.Model, "choices": []any{}, "usage": usage})
	}
	s.send("", "[DONE]")
}

// ---------------------------------------------------------------- anthropic

func fakeAnthropic(w http.ResponseWriter, req *http.Request, log *fakeLog) {
	var body struct {
		Model    string          `json:"model"`
		Stream   bool            `json:"stream"`
		System   json.RawMessage `json:"system"`
		Messages []roleMsg       `json:"messages"`
		Tools    []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	fr := fakeReq{Model: body.Model, Stream: body.Stream, System: len(body.System) > 0 && string(body.System) != "null"}
	fr.addMessages(body.Messages)
	for _, t := range body.Tools {
		fr.Tools = append(fr.Tools, t.Name)
	}
	log.record(req.URL.Path, fr)
	text, tool := answer(fr)
	stop := "end_turn"
	block := map[string]any{"type": "text", "text": text}
	if tool != "" {
		stop = "tool_use"
		block = map[string]any{"type": "tool_use", "id": "toolu_fake", "name": tool, "input": map[string]any{}}
	}
	out := outputTokens(text + tool)
	msg := map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": fr.Model, "content": []any{block},
		"stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": fakeInputTokens, "output_tokens": out}}
	if !fr.Stream {
		writeJSONBody(w, msg)
		return
	}
	s := startSSE(w)
	msg["content"], msg["stop_reason"] = []any{}, nil
	msg["usage"] = map[string]int{"input_tokens": fakeInputTokens, "output_tokens": 1}
	s.send("message_start", map[string]any{"type": "message_start", "message": msg})
	if tool != "" {
		s.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": block})
		s.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}})
	} else {
		s.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		s.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	}
	s.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	s.send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": out}})
	s.send("message_stop", map[string]any{"type": "message_stop"})
}

// ---------------------------------------------------------------- responses

func fakeResponses(w http.ResponseWriter, req *http.Request, log *fakeLog) {
	var body struct {
		Model  string          `json:"model"`
		Stream bool            `json:"stream"`
		Input  json.RawMessage `json:"input"`
		Tools  []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	fr := fakeReq{Model: body.Model, Stream: body.Stream}
	var s string
	if json.Unmarshal(body.Input, &s) == nil {
		fr.addMessages([]roleMsg{{Role: "user", Content: body.Input}})
	} else {
		var items []roleMsg
		if json.Unmarshal(body.Input, &items) == nil {
			fr.addMessages(items)
		}
	}
	for _, t := range body.Tools {
		fr.Tools = append(fr.Tools, t.Name)
	}
	log.record(req.URL.Path, fr)
	text, tool := answer(fr)
	created := time.Now().Unix()
	out := outputTokens(text + tool)
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	item := map[string]any{"type": "message", "id": "msg_fake", "status": "completed", "role": "assistant", "content": []any{part}}
	if tool != "" {
		item = map[string]any{"type": "function_call", "id": "fc_fake", "call_id": "call_fake", "name": tool, "arguments": "{}", "status": "completed"}
	}
	resp := func(status string, output []any, usage any) map[string]any {
		return map[string]any{"id": "resp_fake", "object": "response", "created_at": created, "status": status, "model": fr.Model,
			"output": output, "usage": usage}
	}
	usage := map[string]int{"input_tokens": fakeInputTokens, "output_tokens": out, "total_tokens": fakeInputTokens + out}
	if !fr.Stream {
		writeJSONBody(w, resp("completed", []any{item}, usage))
		return
	}
	st := startSSE(w)
	seq := 0
	ev := func(typ string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = typ, seq
		seq++
		st.send(typ, fields)
	}
	ev("response.created", map[string]any{"response": resp("in_progress", []any{}, nil)})
	ev("response.in_progress", map[string]any{"response": resp("in_progress", []any{}, nil)})
	if tool != "" {
		added := map[string]any{"type": "function_call", "id": "fc_fake", "call_id": "call_fake", "name": tool, "arguments": "", "status": "in_progress"}
		ev("response.output_item.added", map[string]any{"output_index": 0, "item": added})
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_fake", "output_index": 0, "delta": "{}"})
		ev("response.function_call_arguments.done", map[string]any{"item_id": "fc_fake", "output_index": 0, "arguments": "{}"})
	} else {
		added := map[string]any{"type": "message", "id": "msg_fake", "status": "in_progress", "role": "assistant", "content": []any{}}
		empty := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
		ev("response.output_item.added", map[string]any{"output_index": 0, "item": added})
		ev("response.content_part.added", map[string]any{"item_id": "msg_fake", "output_index": 0, "content_index": 0, "part": empty})
		ev("response.output_text.delta", map[string]any{"item_id": "msg_fake", "output_index": 0, "content_index": 0, "delta": text})
		ev("response.output_text.done", map[string]any{"item_id": "msg_fake", "output_index": 0, "content_index": 0, "text": text})
		ev("response.content_part.done", map[string]any{"item_id": "msg_fake", "output_index": 0, "content_index": 0, "part": part})
	}
	ev("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	ev("response.completed", map[string]any{"response": resp("completed", []any{item}, usage)})
}
