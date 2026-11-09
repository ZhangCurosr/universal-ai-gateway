package translate

import (
	"encoding/json"
	"fmt"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

// responsesTopLevel 是 Responses 请求中本转换器处理的顶层字段（spec/10 §8.1、§8.2）。
// 值为 true 的字段被转换或校验；值为 false 的字段按 §8.2 丢弃。
// 未知字段按 spec/10 §1 第 5、6 条处理：reject 模式返回 400，drop 模式丢弃并记单次告警日志。
// text 只接受 format（按 §8.2 校验）与 verbosity（丢弃：目标上游没有等价参数）。
var responsesTopLevel = map[string]bool{
	"model": true, "instructions": true, "input": true, "tools": true, "tool_choice": true,
	"max_output_tokens": true, "temperature": true, "top_p": true, "stream": true,
	"text": true, "store": true, "background": true, "previous_response_id": true, "conversation": true,
	"user": true, "safety_identifier": true,
	// §8.2：丢弃
	"reasoning": false, "parallel_tool_calls": false, "truncation": false, "metadata": false,
	"client_metadata":  false, // T42: Codex CLI 客户端元数据，安全丢弃
	"prompt_cache_key": false, "include": false, "service_tier": false,
}

var responsesItemKeys = map[string]bool{
	"id": true, "type": true, "role": true, "content": true, "call_id": true,
	"name": true, "arguments": true, "output": true, "encrypted_content": true,
	"status": true, "summary": true,
}

// lowerOpenAIResponses 把 OpenAI Responses 创建请求解析为 IR（spec/10 §8.1、§8.2，T25）。
// 有状态特性（RX-01、RX-03）与无法表达的特性返回 apperr.CodeInvalidRequest。
func lowerOpenAIResponses(body []byte, opts Options, directions ...string) (*ir.Request, error) {
	from, to := "openai_responses", "anthropic"
	if len(directions) >= 2 {
		from, to = directions[0], directions[1]
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, invalid("request body must be a JSON object")
	}
	req := &ir.Request{}
	if err := decodeString(top["model"], "model", &req.Model); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, invalid("model: required")
	}
	for k, v := range top {
		if _, known := responsesTopLevel[k]; !known && present(v) {
			if !opts.DropUnknownFields {
				return nil, invalid(fmt.Sprintf("%s is not supported for model %q", k, req.Model))
			}
			logDroppedField(opts, from, to, k)
		}
	}

	// RX-01：有状态特性在转译路径上不支持
	if present(top["previous_response_id"]) {
		return nil, invalid(fmt.Sprintf("previous_response_id is not supported for model %q; send the full input instead", req.Model))
	}
	if present(top["conversation"]) {
		return nil, invalid(fmt.Sprintf("conversation is not supported for model %q; send the full input instead", req.Model))
	}
	// RX-03
	if present(top["background"]) && !isFalse(top["background"]) {
		return nil, invalid(fmt.Sprintf("background is not supported for model %q", req.Model))
	}
	// RX-02：store 视为 false（响应侧写 store:false，T26）；这里只校验类型
	if present(top["store"]) {
		var b bool
		if err := json.Unmarshal(top["store"], &b); err != nil {
			return nil, invalid("store must be a boolean")
		}
	}
	if err := checkResponsesText(top["text"], req.Model); err != nil {
		return nil, err
	}

	if present(top["instructions"]) {
		var s string
		if err := json.Unmarshal(top["instructions"], &s); err != nil {
			return nil, invalid("instructions must be a string")
		}
		if s != "" {
			req.System = append(req.System, ir.TextPart{Text: s})
		}
	}
	if err := lowerResponsesInput(top["input"], req, opts, from, to); err != nil {
		return nil, err
	}
	if err := lowerResponsesTools(top["tools"], top["tool_choice"], req); err != nil {
		return nil, err
	}
	if err := lowerResponsesToolChoice(top["tool_choice"], req); err != nil {
		return nil, err
	}

	if present(top["max_output_tokens"]) {
		if err := decodeInt(top["max_output_tokens"], "max_output_tokens", &req.MaxTokens); err != nil {
			return nil, err
		}
		if req.MaxTokens <= 0 {
			return nil, invalid("max_output_tokens must be a positive integer")
		}
	}
	for name, dst := range map[string]**float64{"temperature": &req.Temperature, "top_p": &req.TopP} {
		if present(top[name]) {
			var f float64
			if err := json.Unmarshal(top[name], &f); err != nil {
				return nil, invalid(name + " must be a number")
			}
			*dst = &f
		}
	}
	if present(top["stream"]) {
		if err := json.Unmarshal(top["stream"], &req.Stream); err != nil {
			return nil, invalid("stream must be a boolean")
		}
	}
	// §8.2：safety_identifier 或 user 写入 IR.User（safety_identifier 优先，它是 user 的替代字段）
	if err := decodeString(top["user"], "user", &req.User); err != nil {
		return nil, err
	}
	var safety string
	if err := decodeString(top["safety_identifier"], "safety_identifier", &safety); err != nil {
		return nil, err
	}
	if safety != "" {
		req.User = safety
	}
	return req, nil
}

// checkResponsesText：text.format 为 json_schema / json_object → 400（§8.2）；format=text 与 verbosity 放行。
func checkResponsesText(raw json.RawMessage, model string) error {
	if !present(raw) {
		return nil
	}
	var text map[string]json.RawMessage
	if err := json.Unmarshal(raw, &text); err != nil {
		return invalid("text must be an object")
	}
	for k, v := range text {
		switch k {
		case "verbosity":
		case "format":
			if !present(v) {
				continue
			}
			var f struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(v, &f); err != nil {
				return invalid("text.format must be an object")
			}
			if f.Type != "text" {
				return invalid(fmt.Sprintf("text.format %q is not supported for model %q", f.Type, model))
			}
		default:
			if present(v) {
				return invalid(fmt.Sprintf("text.%s is not supported for model %q", k, model))
			}
		}
	}
	return nil
}

// responsesItem 是 input[] 中一项本转换器认识的字段。
type responsesItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Output           json.RawMessage `json:"output"`
	EncryptedContent string          `json:"encrypted_content"`
	Status           string          `json:"status"`
	Summary          json.RawMessage `json:"summary"`
}

func lowerResponsesInput(raw json.RawMessage, req *ir.Request, opts Options, from, to string) error {
	if !present(raw) {
		return invalid("input: required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: s}}})
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return invalid("input must be a string or an array of items")
	}
	if len(items) == 0 {
		return invalid("input must not be empty")
	}

	sawConversation := false
	for i, rawItem := range items {
		path := fmt.Sprintf("input[%d]", i)
		var rawKeys map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &rawKeys); err != nil {
			return invalid(path + " must be an object")
		}
		for k, v := range rawKeys {
			if !responsesItemKeys[k] && present(v) {
				if !opts.DropUnknownFields {
					return invalid(fmt.Sprintf("%s.%s is not supported when translating to another protocol", path, k))
				}
				logDroppedField(opts, from, to, "input[]."+k)
			}
		}
		var it responsesItem
		if err := json.Unmarshal(rawItem, &it); err != nil {
			return invalid(path + " must be an object")
		}
		typ := it.Type
		if typ == "" && it.Role != "" {
			typ = "message" // EasyInputMessage：{"role","content"} 可以省略 type
		}

		switch typ {
		case "message":
			switch it.Role {
			case "system", "developer":
				if sawConversation {
					return invalid(path + ": " + it.Role + " messages must come before other input items")
				}
				parts, err := lowerResponsesContent(it.Content, path, false)
				if err != nil {
					return err
				}
				for _, p := range parts {
					t, ok := p.(ir.TextPart)
					if !ok {
						return invalid(path + ": " + it.Role + " messages may only contain text")
					}
					req.System = append(req.System, t)
				}
			case "user", "assistant":
				sawConversation = true
				parts, err := lowerResponsesContent(it.Content, path, it.Role == "user")
				if err != nil {
					return err
				}
				if len(parts) > 0 {
					req.Messages = append(req.Messages, ir.Message{Role: ir.Role(it.Role), Parts: parts})
				}
			default:
				return invalid(path + ".role " + it.Role + " is not supported")
			}

		case "function_call":
			sawConversation = true
			if it.CallID == "" || it.Name == "" {
				return invalid(path + ": call_id and name are required")
			}
			args, err := objectArguments(it.Arguments, path+".arguments")
			if err != nil {
				return err
			}
			appendAssistantPart(req, ir.ToolCallPart{ID: it.CallID, Name: it.Name, Arguments: args})

		case "function_call_output":
			sawConversation = true
			if it.CallID == "" {
				return invalid(path + ".call_id: required")
			}
			content, err := lowerFunctionOutput(it.Output, path+".output")
			if err != nil {
				return err
			}
			result := ir.ToolResultPart{ToolCallID: it.CallID, Content: content}
			if n := len(req.Messages); n > 0 && isToolResultMessage(req.Messages[n-1]) {
				req.Messages[n-1].Parts = append(req.Messages[n-1].Parts, result)
			} else {
				req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Parts: []ir.Part{result}})
			}

		case "reasoning":
			sawConversation = true
			// 合成时按 §1 第 3 条跨厂商丢弃；没有 encrypted_content 的推理项没有可回传的内容
			if it.EncryptedContent != "" {
				appendAssistantPart(req, ir.ThinkingPart{Vendor: ir.VendorOpenAI, Encrypted: it.EncryptedContent})
			}

		case "":
			return invalid(path + ".type: required")
		default:
			return invalid(fmt.Sprintf("%s.type %q is not supported for model %q", path, typ, req.Model))
		}
	}
	if len(req.Messages) == 0 {
		return invalid("input must contain at least one user or assistant item")
	}
	return nil
}

// appendAssistantPart 把块追加到前一条 assistant 消息；前面不是 assistant 消息则新建一条（§8.2）。
func appendAssistantPart(req *ir.Request, p ir.Part) {
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == ir.RoleAssistant {
		req.Messages[n-1].Parts = append(req.Messages[n-1].Parts, p)
		return
	}
	req.Messages = append(req.Messages, ir.Message{Role: ir.RoleAssistant, Parts: []ir.Part{p}})
}

// objectArguments 校验工具调用参数是 JSON 对象字符串（IR 约定）；空字符串视为无参数 {}。
func objectArguments(s, field string) (json.RawMessage, error) {
	if s == "" {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, invalid(field + " must be a JSON object string")
	}
	return json.RawMessage(s), nil
}

// lowerResponsesContent 解析消息 content：字符串，或 input_text / output_text / input_image 部件。
// 图片只允许出现在 user 消息中。
func lowerResponsesContent(raw json.RawMessage, path string, allowImages bool) ([]ir.Part, error) {
	if !present(raw) {
		return nil, invalid(path + ".content: required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Part{ir.TextPart{Text: s}}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		FileID   string `json:"file_id"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, invalid(path + ".content must be a string or an array of content parts")
	}
	out := make([]ir.Part, 0, len(parts))
	for j, p := range parts {
		pp := fmt.Sprintf("%s.content[%d]", path, j)
		switch p.Type {
		case "input_text", "output_text":
			out = append(out, ir.TextPart{Text: p.Text})
		case "input_image":
			if !allowImages {
				return nil, invalid(pp + ": images are only supported in user messages")
			}
			if p.ImageURL == "" {
				return nil, invalid(pp + ": image_url required (file_id is not supported when translating to another protocol)")
			}
			img, err := lowerImageURL(p.ImageURL, pp+".image_url")
			if err != nil {
				return nil, err
			}
			out = append(out, img)
		default:
			return nil, invalid(pp + ".type " + p.Type + " is not supported when translating to another protocol")
		}
	}
	return out, nil
}

// lowerFunctionOutput 解析 function_call_output.output：字符串，或 input_text / input_image 部件数组。
func lowerFunctionOutput(raw json.RawMessage, field string) ([]ir.Part, error) {
	if !present(raw) {
		return nil, invalid(field + ": required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Part{ir.TextPart{Text: s}}, nil
	}
	return lowerResponsesContent(raw, field, true)
}

func lowerResponsesTools(rawTools, rawToolChoice json.RawMessage, req *ir.Request) error {
	if !present(rawTools) {
		return nil
	}
	var tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		// strict：§8.2 丢弃
	}
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return invalid("tools must be an array")
	}
	for i, t := range tools {
		if t.Type != "function" {
			// T42: 非 function 内置工具（namespace, web_search）未在 tool_choice 中显式指定时过滤忽略
			if (t.Type == "namespace" || t.Type == "web_search") && !isToolForcedInChoice(rawToolChoice, t.Type, t.Name) {
				continue
			}
			return invalid(fmt.Sprintf("built-in tool %q is not supported for model %q", t.Type, req.Model))
		}
		if t.Name == "" {
			return invalid(fmt.Sprintf("tools[%d].name: required", i))
		}
		tool := ir.Tool{Name: t.Name, Description: t.Description}
		if present(t.Parameters) {
			tool.InputSchema = t.Parameters
		}
		req.Tools = append(req.Tools, tool)
	}
	return nil
}

func isToolForcedInChoice(rawChoice json.RawMessage, toolType, toolName string) bool {
	if !present(rawChoice) {
		return false
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rawChoice, &obj); err != nil {
		return false
	}
	if obj.Type != "" && obj.Type == toolType {
		return true
	}
	if toolName != "" && obj.Name != "" && obj.Name == toolName {
		return true
	}
	return false
}

func lowerResponsesToolChoice(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			req.ToolChoice = &ir.ToolChoice{Mode: "auto"}
		case "required":
			req.ToolChoice = &ir.ToolChoice{Mode: "any"}
		case "none":
			req.ToolChoice = &ir.ToolChoice{Mode: "none"}
		default:
			return invalid("tool_choice " + s + " is not supported")
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Name == "" {
		return invalid(`tool_choice must be "auto", "required", "none" or {"type":"function","name":...}`)
	}
	req.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: obj.Name}
	return nil
}
