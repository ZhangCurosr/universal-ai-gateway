package openairesp

import (
	"encoding/json"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
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
	// TODO(T09C): 校验 model、input 为非空字符串，max_output_tokens 若存在为正整数，不允许其他字段；
	// Body = {"model","input","instructions"?, "max_output_tokens"?, "store": false}（stream 不设置）。
	return nil, apperr.NotImplemented("T09C")
}

// ToolResult 实现 TL-R02：2xx → 拼接 output[*].content[*] 中 type == "output_text" 的 text；
// 非 2xx → error.message，IsError = true。
func (Ingress) ToolResult(name string, status int, header http.Header, body []byte) (*protocol.ToolResult, error) {
	// TODO(T09C)
	return nil, apperr.NotImplemented("T09C")
}
