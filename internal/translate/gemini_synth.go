package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

// maxGeminiStops 是 generationConfig.stopSequences 的上限（§10.4）。
const maxGeminiStops = 5

// geminiSkipSignature 是 Google 文档给出的占位思考签名（GSIG-02）。
const geminiSkipSignature = "skip_thought_signature_validator"

type geminiOutInline struct {
	MimeType string `json:"mimeType"`
	Data     []byte `json:"data"` // encoding/json 把 []byte 编码为标准 base64
}

type geminiOutCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiOutResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiOutPart struct {
	Text             *string            `json:"text,omitempty"`
	InlineData       *geminiOutInline   `json:"inlineData,omitempty"`
	FunctionCall     *geminiOutCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiOutResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string             `json:"thoughtSignature,omitempty"`
}

type geminiOutContent struct {
	Role  string          `json:"role,omitempty"`
	Parts []geminiOutPart `json:"parts"`
}

type geminiOutDecl struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
}

type geminiOutGenConfig struct {
	MaxOutputTokens int64    `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiOutToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type geminiOutRequest struct {
	Contents          []geminiOutContent `json:"contents"`
	SystemInstruction *geminiOutContent  `json:"systemInstruction,omitempty"`
	Tools             []struct {
		FunctionDeclarations []geminiOutDecl `json:"functionDeclarations"`
	} `json:"tools,omitempty"`
	ToolConfig       *geminiOutToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig *geminiOutGenConfig  `json:"generationConfig,omitempty"`
}

// geminiNeedsSkipSignature 实现 GSIG-02 的模型判断：gemini-1.x / gemini-2.x 不注入，其余一律注入。
func geminiNeedsSkipSignature(model string) bool {
	m := strings.TrimPrefix(model, "models/")
	return !strings.HasPrefix(m, "gemini-1.") && !strings.HasPrefix(m, "gemini-2.")
}

// synthesizeGemini 把 IR 合成为 Gemini generateContent 请求体（spec/10 §10.4）。请求体不含模型名。
func synthesizeGemini(req *ir.Request) ([]byte, error) {
	if len(req.StopSequences) > maxGeminiStops {
		return nil, invalid(fmt.Sprintf("stop_sequences: at most %d are supported by this upstream", maxGeminiStops))
	}
	names := map[string]string{} // 工具调用 ID → 函数名（GID-02）
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if tc, ok := p.(ir.ToolCallPart); ok {
				names[tc.ID] = tc.Name
			}
		}
	}
	inject := geminiNeedsSkipSignature(req.Model)

	out := geminiOutRequest{}
	if len(req.System) > 0 {
		sys := &geminiOutContent{}
		for _, s := range req.System { // cache_control 丢弃
			if s.Text != "" {
				sys.Parts = append(sys.Parts, geminiOutPart{Text: strPtr(s.Text)})
			}
		}
		if len(sys.Parts) > 0 {
			out.SystemInstruction = sys
		}
	}
	for i, m := range req.Messages {
		c, err := geminiContentFor(m, fmt.Sprintf("messages[%d]", i), names, inject)
		if err != nil {
			return nil, err
		}
		if len(c.Parts) > 0 {
			out.Contents = append(out.Contents, c)
		}
	}
	if len(out.Contents) == 0 {
		return nil, invalid("messages must not be empty")
	}

	if len(req.Tools) > 0 {
		decls := make([]geminiOutDecl, 0, len(req.Tools))
		for _, t := range req.Tools { // cache_control 丢弃
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			decls = append(decls, geminiOutDecl{Name: t.Name, Description: t.Description, ParametersJSONSchema: schema})
		}
		out.Tools = append(out.Tools, struct {
			FunctionDeclarations []geminiOutDecl `json:"functionDeclarations"`
		}{FunctionDeclarations: decls})
	}
	if tc := req.ToolChoice; tc != nil {
		cfg := &geminiOutToolConfig{}
		switch tc.Mode {
		case "auto":
			cfg.FunctionCallingConfig.Mode = "AUTO"
		case "any":
			cfg.FunctionCallingConfig.Mode = "ANY"
		case "none":
			cfg.FunctionCallingConfig.Mode = "NONE"
		case "tool":
			cfg.FunctionCallingConfig.Mode = "ANY"
			cfg.FunctionCallingConfig.AllowedFunctionNames = []string{tc.Name}
		default:
			return nil, invalid(fmt.Sprintf("tool_choice %q is not supported by this upstream", tc.Mode))
		}
		out.ToolConfig = cfg
	}
	if req.MaxTokens > 0 || req.Temperature != nil || req.TopP != nil || len(req.StopSequences) > 0 {
		out.GenerationConfig = &geminiOutGenConfig{
			MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP, StopSequences: req.StopSequences,
		}
	}
	// User：Gemini 没有对应字段，丢弃
	return json.Marshal(out)
}

func strPtr(s string) *string { return &s }

// geminiContentFor 合成一条消息。user 消息中的工具结果放在其余 part 之前（§10.4）。
func geminiContentFor(m ir.Message, path string, names map[string]string, inject bool) (geminiOutContent, error) {
	c := geminiOutContent{Role: "user"}
	if m.Role == ir.RoleAssistant {
		c.Role = "model"
	}
	var results, rest []geminiOutPart
	signed := false
	for j, p := range m.Parts {
		pp := fmt.Sprintf("%s.content[%d]", path, j)
		switch v := p.(type) {
		case ir.TextPart:
			if v.Text != "" {
				rest = append(rest, geminiOutPart{Text: strPtr(v.Text)})
			}
		case ir.ImagePart:
			if v.URL != "" || len(v.Data) == 0 {
				return c, invalid(pp + ": image URLs are not supported by this upstream; send base64 data instead")
			}
			rest = append(rest, geminiOutPart{InlineData: &geminiOutInline{MimeType: v.MediaType, Data: v.Data}})
		case ir.ToolCallPart:
			args := v.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			part := geminiOutPart{FunctionCall: &geminiOutCall{Name: v.Name, Args: args}}
			if inject && !signed && m.Role == ir.RoleAssistant {
				part.ThoughtSignature = geminiSkipSignature
				signed = true
			}
			rest = append(rest, part)
		case ir.ToolResultPart:
			name, ok := names[v.ToolCallID]
			if !ok {
				return c, invalid(fmt.Sprintf("tool result %q has no matching tool call", v.ToolCallID))
			}
			var sb strings.Builder
			for _, rp := range v.Content {
				switch r := rp.(type) {
				case ir.TextPart:
					sb.WriteString(r.Text)
				default:
					return c, invalid(pp + ": images in tool results are not supported by this upstream")
				}
			}
			key := "output"
			if v.IsError {
				key = "error"
			}
			results = append(results, geminiOutPart{FunctionResponse: &geminiOutResponse{Name: name, Response: map[string]any{key: sb.String()}}})
		case ir.ThinkingPart:
			// 来源都不是 Gemini（§1 第 3 条），丢弃
		}
	}
	c.Parts = append(results, rest...)
	return c, nil
}
