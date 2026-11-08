package openairesp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// ToolAskModel 是把模型调用暴露给 AI 的工具名（spec/14 §4.1）。
const ToolAskModel = "ask_model"

var askModelSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "model": {"type": "string", "description": "Service name to call; must be one of the available models listed in the tool description."},
    "input": {"type": "string", "description": "The prompt or question to send to the model."},
    "instructions": {"type": "string", "description": "Optional system instructions."},
    "max_output_tokens": {"type": "integer", "minimum": 1, "description": "Optional upper bound on generated tokens."}
  },
  "required": ["model", "input"],
  "additionalProperties": false
}`)

// Tools 返回工具 ask_model。
func (Ingress) Tools() []protocol.Tool {
	return []protocol.Tool{{
		Name:        ToolAskModel,
		Title:       "Ask another model",
		Description: "Send a single prompt to another AI model through the gateway and return its text answer. Use it for second opinions, delegation, or models with different strengths.",
		InputSchema: askModelSchema,
		ReadOnly:    true,
	}}
}

// ToolRequest 实现 TL-R01：转换为非流式、store=false 的 responses.create 请求。
func (Ingress) ToolRequest(name string, args json.RawMessage) (*protocol.ToolRequest, error) {
	if name != ToolAskModel {
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool: "+name)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "arguments must be a JSON object")
	}
	for k := range raw {
		switch k {
		case "model", "input", "instructions", "max_output_tokens":
		default:
			return nil, apperr.New(apperr.CodeInvalidRequest, "unsupported parameter: "+k)
		}
	}
	modelRaw, ok := raw["model"]
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidRequest, "model is required")
	}
	var model string
	if err := json.Unmarshal(modelRaw, &model); err != nil || model == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "model must be a non-empty string")
	}
	inputRaw, ok := raw["input"]
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidRequest, "input is required")
	}
	var input string
	if err := json.Unmarshal(inputRaw, &input); err != nil || input == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "input must be a non-empty string")
	}

	bodyMap := map[string]any{
		"model": model,
		"input": input,
		"store": false,
	}

	if instRaw, ok := raw["instructions"]; ok {
		var inst string
		if err := json.Unmarshal(instRaw, &inst); err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "instructions must be a string")
		}
		bodyMap["instructions"] = inst
	}

	if maxRaw, ok := raw["max_output_tokens"]; ok {
		var maxTokens int64
		if err := json.Unmarshal(maxRaw, &maxTokens); err != nil || maxTokens <= 0 {
			return nil, apperr.New(apperr.CodeInvalidRequest, "max_output_tokens must be a positive integer")
		}
		bodyMap["max_output_tokens"] = maxTokens
	}

	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, "failed to marshal request body")
	}

	return &protocol.ToolRequest{
		Op:   OpCreate,
		Body: bodyBytes,
	}, nil
}

// ToolResult 实现 TL-R02：2xx → 拼接 output[*].content[*] 中 type == "output_text" 的 text；
// 非 2xx → error.message，IsError = true。
func (Ingress) ToolResult(name string, status int, header http.Header, body []byte) (*protocol.ToolResult, error) {
	if name != ToolAskModel {
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool: "+name)
	}
	if status < 200 || status >= 300 {
		errMsg, ok := jsonx.GetString(body, "error", "message")
		if !ok || errMsg == "" {
			errMsg = string(body)
		}
		return protocol.TextResult(errMsg, true), nil
	}
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var sb strings.Builder
	for _, item := range resp.Output {
		if item.Type == "message" {
			for _, c := range item.Content {
				if c.Type == "output_text" {
					sb.WriteString(c.Text)
				}
			}
		}
	}
	return protocol.TextResult(sb.String(), false), nil
}
