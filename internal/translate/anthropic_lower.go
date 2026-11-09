package translate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

// anthropicTopLevel 是 Anthropic Messages 请求体中本转换器处理的顶层字段（spec/10 §6.1）。
// 未知字段按 spec/10 §1 第 5、6 条处理：reject 模式返回 400，drop 模式丢弃并记单次告警日志。
var anthropicTopLevel = map[string]bool{
	"model": true, "max_tokens": true, "system": true, "messages": true,
	"tools": true, "tool_choice": true, "stop_sequences": true,
	"temperature": true, "top_p": true, "stream": true, "metadata": true,
	"thinking": true, "service_tier": true, "context_management": true, // 丢弃
	"top_k": true, // T42: spec/10 §6.1 丢弃
}

// anthropicRejected 是 spec/10 §6.1 明确不支持的顶层字段（始终 400，不受 unknown_fields 影响）。
var anthropicRejected = map[string]bool{
	"mcp_servers": true,
	"container":   true,
}

// lowerAnthropic 把 Anthropic Messages 请求体解析为 IR（spec/10 §6.1，T23）。
// 任何无法表达的特性返回 apperr.CodeInvalidRequest。
//
// 内容块与工具定义中未列出的键（如 text.citations 以外的提示性字段）按 spec/10 §1 第 5 条
// 始终丢弃，避免客户端新增的提示字段让请求整体失败。
func lowerAnthropic(body []byte, opts Options, directions ...string) (*ir.Request, error) {
	from, to := "anthropic", "openai_chat"
	if len(directions) >= 2 {
		from, to = directions[0], directions[1]
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, invalid("request body must be a JSON object")
	}
	for k, v := range top {
		if anthropicRejected[k] && present(v) {
			return nil, invalid(k + " is not supported when translating to another protocol")
		}
		if !anthropicTopLevel[k] && present(v) {
			if !opts.DropUnknownFields {
				return nil, invalid(k + " is not supported when translating to another protocol")
			}
			logDroppedField(opts, from, to, k)
		}
	}

	req := &ir.Request{}
	if err := decodeString(top["model"], "model", &req.Model); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, invalid("model: required")
	}
	if !present(top["max_tokens"]) {
		return nil, invalid("max_tokens: required")
	}
	if err := decodeInt(top["max_tokens"], "max_tokens", &req.MaxTokens); err != nil {
		return nil, err
	}
	if req.MaxTokens <= 0 {
		return nil, invalid("max_tokens must be a positive integer")
	}

	if err := lowerAnthropicSystem(top["system"], req); err != nil {
		return nil, err
	}
	if err := lowerAnthropicMessages(top["messages"], req, opts, from, to); err != nil {
		return nil, err
	}
	if err := lowerAnthropicTools(top["tools"], req); err != nil {
		return nil, err
	}
	if err := lowerAnthropicToolChoice(top["tool_choice"], req); err != nil {
		return nil, err
	}

	if present(top["stop_sequences"]) {
		if err := json.Unmarshal(top["stop_sequences"], &req.StopSequences); err != nil {
			return nil, invalid("stop_sequences must be an array of strings")
		}
	}
	for _, f := range []struct {
		name string
		dst  **float64
	}{{"temperature", &req.Temperature}, {"top_p", &req.TopP}} {
		if present(top[f.name]) {
			var v float64
			if err := json.Unmarshal(top[f.name], &v); err != nil {
				return nil, invalid(f.name + " must be a number")
			}
			*f.dst = &v
		}
	}
	if present(top["stream"]) {
		if err := json.Unmarshal(top["stream"], &req.Stream); err != nil {
			return nil, invalid("stream must be a boolean")
		}
	}
	if present(top["metadata"]) {
		var md struct {
			UserID *string `json:"user_id"`
		}
		if err := json.Unmarshal(top["metadata"], &md); err != nil {
			return nil, invalid("metadata must be an object")
		}
		if md.UserID != nil {
			req.User = *md.UserID // metadata 其他字段丢弃
		}
	}
	// thinking / service_tier / context_management：§6.1 丢弃
	return req, nil
}

func lowerAnthropicSystem(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		req.System = []ir.TextPart{{Text: s}}
		return nil
	}
	var blocks []anthropicInBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return invalid("system must be a string or an array of text blocks")
	}
	for i, b := range blocks {
		if b.Type != "text" {
			return invalid(fmt.Sprintf("system[%d].type %s is not supported", i, b.Type))
		}
		req.System = append(req.System, ir.TextPart{Text: b.Text, CacheControl: b.cache()})
	}
	return nil
}

// anthropicInBlock 是入站内容块中本转换器读取的字段。
type anthropicInBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl *anthropicCache `json:"cache_control"`
	Source       *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
}

func (b anthropicInBlock) cache() *ir.CacheControl {
	if b.CacheControl == nil {
		return nil
	}
	return &ir.CacheControl{Type: b.CacheControl.Type, TTL: b.CacheControl.TTL}
}

func lowerAnthropicMessages(raw json.RawMessage, req *ir.Request, opts Options, from, to string) error {
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
	for i, item := range list {
		path := fmt.Sprintf("messages[%d]", i)
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(item, &keys); err != nil {
			return invalid(path + " must be an object")
		}
		for k, v := range keys {
			if k != "role" && k != "content" && present(v) {
				if !opts.DropUnknownFields {
					return invalid(path + "." + k + " is not supported when translating to another protocol")
				}
				logDroppedField(opts, from, to, "messages[]."+k)
			}
		}
		var role string
		if err := decodeString(keys["role"], path+".role", &role); err != nil {
			return err
		}
		if role != string(ir.RoleUser) && role != string(ir.RoleAssistant) {
			return invalid(path + ".role must be user or assistant")
		}
		parts, err := lowerAnthropicContent(keys["content"], path, ir.Role(role))
		if err != nil {
			return err
		}
		req.Messages = append(req.Messages, ir.Message{Role: ir.Role(role), Parts: parts})
	}
	return nil
}

func lowerAnthropicContent(raw json.RawMessage, path string, role ir.Role) ([]ir.Part, error) {
	if !present(raw) {
		return nil, invalid(path + ".content: required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Part{ir.TextPart{Text: s}}, nil
	}
	var blocks []anthropicInBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, invalid(path + ".content must be a string or an array of content blocks")
	}
	out := make([]ir.Part, 0, len(blocks))
	for j, b := range blocks {
		bp := fmt.Sprintf("%s.content[%d]", path, j)
		onlyFor := func(want ir.Role) error {
			if role != want {
				return invalid(bp + ": " + b.Type + " blocks are only allowed in " + string(want) + " messages")
			}
			return nil
		}
		switch b.Type {
		case "text":
			out = append(out, ir.TextPart{Text: b.Text, CacheControl: b.cache()}) // citations 丢弃
		case "image":
			img, err := lowerAnthropicImage(b, bp)
			if err != nil {
				return nil, err
			}
			out = append(out, img)
		case "tool_use":
			if err := onlyFor(ir.RoleAssistant); err != nil {
				return nil, err
			}
			args := bytes.TrimSpace(b.Input)
			if b.ID == "" || b.Name == "" {
				return nil, invalid(bp + ": id and name are required")
			}
			if len(args) == 0 || args[0] != '{' {
				return nil, invalid(bp + ".input must be a JSON object")
			}
			out = append(out, ir.ToolCallPart{ID: b.ID, Name: b.Name, Arguments: json.RawMessage(args)})
		case "tool_result":
			if err := onlyFor(ir.RoleUser); err != nil {
				return nil, err
			}
			if b.ToolUseID == "" {
				return nil, invalid(bp + ".tool_use_id: required")
			}
			content, err := lowerAnthropicToolResult(b.Content, bp)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.ToolResultPart{ToolCallID: b.ToolUseID, Content: content, IsError: b.IsError})
		case "thinking":
			if err := onlyFor(ir.RoleAssistant); err != nil {
				return nil, err
			}
			out = append(out, ir.ThinkingPart{Vendor: ir.VendorAnthropic, Text: b.Thinking, Signature: b.Signature})
		case "redacted_thinking":
			if err := onlyFor(ir.RoleAssistant); err != nil {
				return nil, err
			}
			out = append(out, ir.ThinkingPart{Vendor: ir.VendorAnthropic, Redacted: true, RedactedData: b.Data})
		default:
			return nil, invalid(bp + ".type " + b.Type + " is not supported when translating to another protocol")
		}
	}
	if len(out) == 0 {
		return nil, invalid(path + ".content must not be empty")
	}
	return out, nil
}

// lowerAnthropicToolResult：content 为字符串，或只含 text / image 块的数组；缺省为空结果。
func lowerAnthropicToolResult(raw json.RawMessage, path string) ([]ir.Part, error) {
	if !present(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Part{ir.TextPart{Text: s}}, nil
	}
	var blocks []anthropicInBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, invalid(path + ".content must be a string or an array of text/image blocks")
	}
	out := make([]ir.Part, 0, len(blocks))
	for k, b := range blocks {
		bp := fmt.Sprintf("%s.content[%d]", path, k)
		switch b.Type {
		case "text":
			out = append(out, ir.TextPart{Text: b.Text, CacheControl: b.cache()})
		case "image":
			img, err := lowerAnthropicImage(b, bp)
			if err != nil {
				return nil, err
			}
			out = append(out, img)
		default:
			return nil, invalid(bp + ".type " + b.Type + " is not supported inside tool_result")
		}
	}
	return out, nil
}

func lowerAnthropicImage(b anthropicInBlock, path string) (ir.ImagePart, error) {
	if b.Source == nil {
		return ir.ImagePart{}, invalid(path + ".source: required")
	}
	switch b.Source.Type {
	case "base64":
		if !strings.HasPrefix(b.Source.MediaType, "image/") {
			return ir.ImagePart{}, invalid(path + ".source.media_type must be an image type")
		}
		data, err := base64.StdEncoding.DecodeString(b.Source.Data)
		if err != nil {
			return ir.ImagePart{}, invalid(path + ".source.data contains invalid base64 data")
		}
		return ir.ImagePart{MediaType: b.Source.MediaType, Data: data}, nil
	case "url":
		if !strings.HasPrefix(b.Source.URL, "https://") && !strings.HasPrefix(b.Source.URL, "http://") {
			return ir.ImagePart{}, invalid(path + ".source.url must be an http(s) URL")
		}
		return ir.ImagePart{URL: b.Source.URL}, nil
	default:
		return ir.ImagePart{}, invalid(path + ".source.type " + b.Source.Type + " is not supported when translating to another protocol")
	}
}

func lowerAnthropicTools(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var tools []struct {
		Type         string          `json:"type"`
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"input_schema"`
		CacheControl *anthropicCache `json:"cache_control"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return invalid("tools must be an array")
	}
	for i, t := range tools {
		if t.Type != "" && t.Type != "custom" {
			return invalid(fmt.Sprintf("built-in tool %q is not supported for model %q", t.Type, req.Model))
		}
		if t.Name == "" {
			return invalid(fmt.Sprintf("tools[%d].name: required", i))
		}
		tool := ir.Tool{Name: t.Name, Description: t.Description}
		if present(t.InputSchema) {
			tool.InputSchema = t.InputSchema
		}
		if t.CacheControl != nil {
			tool.CacheControl = &ir.CacheControl{Type: t.CacheControl.Type, TTL: t.CacheControl.TTL}
		}
		req.Tools = append(req.Tools, tool)
	}
	return nil
}

func lowerAnthropicToolChoice(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return invalid("tool_choice must be an object")
	}
	if tc.DisableParallelToolUse {
		return invalid("tool_choice.disable_parallel_tool_use is not supported when translating to another protocol")
	}
	switch tc.Type {
	case "auto", "any", "none":
		req.ToolChoice = &ir.ToolChoice{Mode: tc.Type}
	case "tool":
		if tc.Name == "" {
			return invalid("tool_choice.name: required when type is tool")
		}
		req.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: tc.Name}
	default:
		return invalid("tool_choice.type " + tc.Type + " is not supported")
	}
	return nil
}
