package translate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

// chatTopLevel 是 OpenAI Chat 请求体中本转换器认识的顶层字段（spec/10 §3）。
//
// SPEC-GAP(T20): §3 只列出了要拒绝的字段，没有说明其他未列出的字段怎么处理。按 §1 第 2 条
// 「禁止默默编造等价物」与 AGENTS §2.7「最保守」，这里对未列出的字段一律返回 400，并在
// 消息中写出字段名；只有下面两类例外：
//   - stream_options：只影响响应形态（include_usage），由响应转换器（T22）读取原始请求体；
//   - parallel_tool_calls 为 true：与 IR / Anthropic 的默认行为相同，不需要表达。
var chatTopLevel = map[string]bool{
	"model": true, "messages": true, "tools": true, "tool_choice": true,
	"max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "top_p": true, "stop": true, "stream": true, "stream_options": true,
	"user": true, "n": true, "parallel_tool_calls": true,
}

// chatRejected 是 §3 明确不支持的字段（出现且不为 null / false 时 400）。
var chatRejected = []string{"response_format", "logprobs", "top_logprobs", "audio", "modalities"}

// lowerOpenAIChat 把 OpenAI Chat Completions 请求体解析为 IR（spec/10 §3，T20）。
// 任何无法表达的特性返回 apperr.CodeInvalidRequest。
func lowerOpenAIChat(body []byte) (*ir.Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, invalid("request body must be a JSON object")
	}

	for _, k := range chatRejected {
		if present(top[k]) && !isFalse(top[k]) {
			return nil, invalid(k + " is not supported when translating to another protocol")
		}
	}
	for k, v := range top {
		if !chatTopLevel[k] && !slices.Contains(chatRejected, k) && present(v) {
			return nil, invalid(k + " is not supported when translating to another protocol")
		}
	}

	req := &ir.Request{}
	if err := decodeString(top["model"], "model", &req.Model); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, invalid("model: required")
	}

	if present(top["n"]) {
		var n int64
		if err := decodeInt(top["n"], "n", &n); err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, invalid("n > 1 is not supported")
		}
	}
	if present(top["parallel_tool_calls"]) {
		var p bool
		if err := json.Unmarshal(top["parallel_tool_calls"], &p); err != nil {
			return nil, invalid("parallel_tool_calls must be a boolean")
		}
		if !p {
			return nil, invalid("parallel_tool_calls=false is not supported when translating to another protocol")
		}
	}
	if present(top["stream_options"]) {
		var so map[string]json.RawMessage
		if err := json.Unmarshal(top["stream_options"], &so); err != nil {
			return nil, invalid("stream_options must be an object")
		}
	}

	if err := lowerChatMessages(top["messages"], req); err != nil {
		return nil, err
	}
	if err := lowerChatTools(top["tools"], req); err != nil {
		return nil, err
	}
	if err := lowerChatToolChoice(top["tool_choice"], req); err != nil {
		return nil, err
	}

	// max_completion_tokens 优先，否则 max_tokens
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if present(top[k]) {
			if err := decodeInt(top[k], k, &req.MaxTokens); err != nil {
				return nil, err
			}
			if req.MaxTokens <= 0 {
				return nil, invalid(k + " must be a positive integer")
			}
			break
		}
	}
	if present(top["temperature"]) {
		var f float64
		if err := json.Unmarshal(top["temperature"], &f); err != nil {
			return nil, invalid("temperature must be a number")
		}
		req.Temperature = &f
	}
	if present(top["top_p"]) {
		var f float64
		if err := json.Unmarshal(top["top_p"], &f); err != nil {
			return nil, invalid("top_p must be a number")
		}
		req.TopP = &f
	}
	if present(top["stop"]) {
		stops, err := decodeStringOrArray(top["stop"], "stop")
		if err != nil {
			return nil, err
		}
		req.StopSequences = stops
	}
	if present(top["stream"]) {
		if err := json.Unmarshal(top["stream"], &req.Stream); err != nil {
			return nil, invalid("stream must be a boolean")
		}
	}
	if err := decodeString(top["user"], "user", &req.User); err != nil {
		return nil, err
	}
	return req, nil
}

// chatMessage 是一条 OpenAI 消息中本转换器认识的字段。
type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	Name             json.RawMessage `json:"name"`              // spec/10 §3：参与者名丢弃（IR / Anthropic 无对应字段）
	ReasoningContent json.RawMessage `json:"reasoning_content"` // §3：丢弃
	Refusal          json.RawMessage `json:"refusal"`
}

var chatMessageKeys = map[string]bool{
	"role": true, "content": true, "tool_calls": true, "tool_call_id": true,
	"name": true, "reasoning_content": true, "refusal": true,
}

func lowerChatMessages(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return invalid("messages: required")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return invalid("messages must be an array")
	}
	if len(list) == 0 {
		return invalid("messages must not be empty")
	}

	sawConversation := false
	for i, item := range list {
		path := fmt.Sprintf("messages[%d]", i)
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(item, &keys); err != nil {
			return invalid(path + " must be an object")
		}
		for k, v := range keys {
			if !chatMessageKeys[k] && present(v) {
				return invalid(path + "." + k + " is not supported when translating to another protocol")
			}
		}
		var m chatMessage
		if err := json.Unmarshal(item, &m); err != nil {
			return invalid(path + ": " + err.Error())
		}
		if present(m.Refusal) {
			return invalid(path + ".refusal is not supported when translating to another protocol")
		}

		switch m.Role {
		case "system", "developer":
			if sawConversation {
				return invalid(path + ": " + m.Role + " messages must come before user and assistant messages")
			}
			parts, err := lowerChatTextOnly(m.Content, path)
			if err != nil {
				return err
			}
			req.System = append(req.System, parts...)

		case "user":
			sawConversation = true
			parts, err := lowerChatUserContent(m.Content, path)
			if err != nil {
				return err
			}
			req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Parts: parts})

		case "assistant":
			sawConversation = true
			msg, err := lowerChatAssistant(m, path)
			if err != nil {
				return err
			}
			req.Messages = append(req.Messages, msg)

		case "tool":
			sawConversation = true
			if m.ToolCallID == "" {
				return invalid(path + ".tool_call_id: required")
			}
			content, err := lowerChatTextOnly(m.Content, path)
			if err != nil {
				return err
			}
			result := ir.ToolResultPart{ToolCallID: m.ToolCallID, Content: textParts(content)}
			// 连续的多条 tool 消息合并到同一条 user 消息（§3）
			if n := len(req.Messages); n > 0 && isToolResultMessage(req.Messages[n-1]) {
				req.Messages[n-1].Parts = append(req.Messages[n-1].Parts, result)
			} else {
				req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Parts: []ir.Part{result}})
			}

		case "":
			return invalid(path + ".role: required")
		default:
			return invalid(path + ".role " + m.Role + " is not supported")
		}
	}
	if len(req.Messages) == 0 {
		return invalid("messages must contain at least one user or assistant message")
	}
	return nil
}

// isToolResultMessage 报告 m 是否是由 tool 消息生成的 user 消息（只含 ToolResultPart）。
func isToolResultMessage(m ir.Message) bool {
	if m.Role != ir.RoleUser || len(m.Parts) == 0 {
		return false
	}
	for _, p := range m.Parts {
		if _, ok := p.(ir.ToolResultPart); !ok {
			return false
		}
	}
	return true
}

func lowerChatAssistant(m chatMessage, path string) (ir.Message, error) {
	msg := ir.Message{Role: ir.RoleAssistant}
	if present(m.Content) {
		parts, err := lowerChatTextOnly(m.Content, path)
		if err != nil {
			return msg, err
		}
		for _, p := range parts {
			if p.Text != "" { // 只有工具调用时 content 常为 ""，不生成空文本块
				msg.Parts = append(msg.Parts, p)
			}
		}
	}
	// reasoning_content：§3 丢弃（思考内容只能回传给签发它的厂商，§1 第 3 条）

	if present(m.ToolCalls) {
		var calls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
			return msg, invalid(path + ".tool_calls must be an array of function calls")
		}
		for j, c := range calls {
			cp := fmt.Sprintf("%s.tool_calls[%d]", path, j)
			if c.Type != "" && c.Type != "function" {
				return msg, invalid(cp + ".type " + c.Type + " is not supported")
			}
			if c.ID == "" || c.Function.Name == "" {
				return msg, invalid(cp + ": id and function.name are required")
			}
			args := bytes.TrimSpace([]byte(c.Function.Arguments))
			if !json.Valid(args) || len(args) == 0 || args[0] != '{' {
				return msg, invalid(cp + ".function.arguments must be a JSON object string")
			}
			msg.Parts = append(msg.Parts, ir.ToolCallPart{ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(args)})
		}
	}
	if len(msg.Parts) == 0 {
		return msg, invalid(path + ": assistant message must have content or tool_calls")
	}
	return msg, nil
}

// lowerChatTextOnly 解析只允许文本的 content：字符串，或只含 type=text 的数组。
func lowerChatTextOnly(raw json.RawMessage, path string) ([]ir.TextPart, error) {
	if !present(raw) {
		return nil, invalid(path + ".content: required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.TextPart{{Text: s}}, nil
	}
	parts, err := decodeContentArray(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]ir.TextPart, 0, len(parts))
	for j, p := range parts {
		if p.Type != "text" {
			return nil, invalid(fmt.Sprintf("%s.content[%d].type %s is not supported here", path, j, p.Type))
		}
		out = append(out, ir.TextPart{Text: p.Text})
	}
	return out, nil
}

func lowerChatUserContent(raw json.RawMessage, path string) ([]ir.Part, error) {
	if !present(raw) {
		return nil, invalid(path + ".content: required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Part{ir.TextPart{Text: s}}, nil
	}
	parts, err := decodeContentArray(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]ir.Part, 0, len(parts))
	for j, p := range parts {
		pp := fmt.Sprintf("%s.content[%d]", path, j)
		switch p.Type {
		case "text":
			out = append(out, ir.TextPart{Text: p.Text})
		case "image_url":
			img, err := lowerImageURL(p.ImageURL.URL, pp+".image_url.url")
			if err != nil {
				return nil, err
			}
			out = append(out, img)
		default:
			return nil, invalid(pp + ".type " + p.Type + " is not supported when translating to another protocol")
		}
	}
	if len(out) == 0 {
		return nil, invalid(path + ".content must not be empty")
	}
	return out, nil
}

type chatContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

func decodeContentArray(raw json.RawMessage, path string) ([]chatContentPart, error) {
	var parts []chatContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, invalid(path + ".content must be a string or an array of content parts")
	}
	return parts, nil
}

// lowerImageURL：data: URL 解码为 Data + MediaType；http(s) URL 放 URL（§3、§8.2）。field 是错误消息中的字段路径。
func lowerImageURL(u, field string) (ir.ImagePart, error) {
	switch {
	case strings.HasPrefix(u, "data:"):
		meta, data, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ",")
		mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
		if !ok || !isBase64 || mediaType == "" || !strings.HasPrefix(mediaType, "image/") {
			return ir.ImagePart{}, invalid(field + " must be a base64 data URL of an image")
		}
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return ir.ImagePart{}, invalid(field + " contains invalid base64 data")
		}
		return ir.ImagePart{MediaType: mediaType, Data: b}, nil
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		return ir.ImagePart{URL: u}, nil
	default:
		return ir.ImagePart{}, invalid(field + " must be a data: or http(s) URL")
	}
}

func lowerChatTools(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return invalid("tools must be an array")
	}
	for i, t := range tools {
		path := fmt.Sprintf("tools[%d]", i)
		if t.Type != "function" {
			return invalid(path + ".type " + t.Type + " is not supported (only function)")
		}
		if t.Function.Name == "" {
			return invalid(path + ".function.name: required")
		}
		if t.Function.Strict != nil && *t.Function.Strict {
			return invalid(path + ".function.strict is not supported when translating to another protocol")
		}
		tool := ir.Tool{Name: t.Function.Name, Description: t.Function.Description}
		if present(t.Function.Parameters) {
			tool.InputSchema = t.Function.Parameters
		}
		req.Tools = append(req.Tools, tool)
	}
	return nil
}

func lowerChatToolChoice(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			req.ToolChoice = &ir.ToolChoice{Mode: "auto"}
		case "none":
			req.ToolChoice = &ir.ToolChoice{Mode: "none"}
		case "required":
			req.ToolChoice = &ir.ToolChoice{Mode: "any"}
		default:
			return invalid("tool_choice " + s + " is not supported")
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
		return invalid(`tool_choice must be "auto", "none", "required" or {"type":"function","function":{"name":...}}`)
	}
	req.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: obj.Function.Name}
	return nil
}

// ---- 小工具 ----

func invalid(msg string) error { return apperr.New(apperr.CodeInvalidRequest, msg) }

// present 报告字段存在且不是 JSON null。
func present(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

func isFalse(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("false")) }

func decodeString(raw json.RawMessage, name string, dst *string) error {
	if !present(raw) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return invalid(name + " must be a string")
	}
	return nil
}

func decodeInt(raw json.RawMessage, name string, dst *int64) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return invalid(name + " must be an integer")
	}
	return nil
}

func decodeStringOrArray(raw json.RawMessage, name string) ([]string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, invalid(name + " must be a string or an array of strings")
	}
	return list, nil
}

func textParts(ts []ir.TextPart) []ir.Part {
	out := make([]ir.Part, len(ts))
	for i, t := range ts {
		out[i] = t
	}
	return out
}
