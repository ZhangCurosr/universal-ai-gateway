package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
)

// maxChatStops 是 OpenAI Chat `stop` 数组的上限（§6.2）。
const maxChatStops = 4

type chatOutMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // string、[]chatOutPart 或 nil（null）
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatOutPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type chatOutTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatOutRequest struct {
	Model         string           `json:"model"`
	Messages      []chatOutMessage `json:"messages"`
	Tools         []chatOutTool    `json:"tools,omitempty"`
	ToolChoice    any              `json:"tool_choice,omitempty"`
	MaxTokens     int64            `json:"max_tokens,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	Stop          []string         `json:"stop,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	User string `json:"user,omitempty"`
}

// synthesizeOpenAIChat 把 IR 合成为 OpenAI Chat Completions 请求体（spec/10 §6.2，T23；T27 复用）。
func synthesizeOpenAIChat(req *ir.Request) ([]byte, error) {
	if len(req.StopSequences) > maxChatStops {
		return nil, invalid(fmt.Sprintf("stop_sequences: at most %d are supported by this upstream", maxChatStops))
	}
	out := chatOutRequest{
		Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP,
		Stop: req.StopSequences, Stream: req.Stream, User: req.User,
	}
	if req.Stream {
		// 显式要求 usage：PT-03 不再注入，FWD-02 不会过滤 usage chunk，响应转换器能拿到用量（§6.2）
		out.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}

	for _, s := range req.System { // cache_control 丢弃
		out.Messages = append(out.Messages, chatOutMessage{Role: "system", Content: s.Text})
	}
	for i, m := range req.Messages {
		msgs, err := chatMessagesFor(m, fmt.Sprintf("messages[%d]", i))
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msgs...)
	}
	if len(out.Messages) == 0 {
		return nil, invalid("messages must not be empty")
	}

	for _, t := range req.Tools { // cache_control 丢弃
		ct := chatOutTool{Type: "function"}
		ct.Function.Name, ct.Function.Description, ct.Function.Parameters = t.Name, t.Description, t.InputSchema
		if len(ct.Function.Parameters) == 0 {
			ct.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, ct)
	}
	if req.ToolChoice != nil && len(out.Tools) > 0 { // 没有 tools 时上游会拒绝 tool_choice
		switch req.ToolChoice.Mode {
		case "auto", "none":
			out.ToolChoice = req.ToolChoice.Mode
		case "any":
			out.ToolChoice = "required"
		case "tool":
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": req.ToolChoice.Name}}
		default:
			return nil, invalid("tool_choice " + req.ToolChoice.Mode + " is not supported")
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	return b, nil
}

// chatMessagesFor 把一条 IR 消息展开为若干 Chat 消息；只含思考块的 assistant 消息返回空。
func chatMessagesFor(m ir.Message, path string) ([]chatOutMessage, error) {
	if m.Role == ir.RoleAssistant {
		return chatAssistant(m, path)
	}
	var out []chatOutMessage
	var rest []ir.Part
	for j, p := range m.Parts {
		tr, ok := p.(ir.ToolResultPart)
		if !ok {
			rest = append(rest, p)
			continue
		}
		var sb strings.Builder
		for _, c := range tr.Content {
			t, ok := c.(ir.TextPart)
			if !ok {
				return nil, invalid(fmt.Sprintf("%s.content[%d]: images in tool results are not supported by this upstream", path, j))
			}
			sb.WriteString(t.Text)
		}
		// is_error 丢弃：错误文本已在 content 中（§6.2）
		out = append(out, chatOutMessage{Role: "tool", ToolCallID: tr.ToolCallID, Content: sb.String()})
	}
	if len(rest) == 0 {
		return out, nil
	}
	if t, ok := rest[0].(ir.TextPart); ok && len(rest) == 1 {
		return append(out, chatOutMessage{Role: "user", Content: t.Text}), nil
	}
	parts := make([]chatOutPart, 0, len(rest))
	for j, p := range rest {
		switch v := p.(type) {
		case ir.TextPart:
			parts = append(parts, chatOutPart{Type: "text", Text: v.Text})
		case ir.ImagePart:
			u, err := chatImageURL(v, fmt.Sprintf("%s.content[%d]", path, j))
			if err != nil {
				return nil, err
			}
			parts = append(parts, chatOutPart{Type: "image_url", ImageURL: &struct {
				URL string `json:"url"`
			}{URL: u}})
		case ir.ThinkingPart:
			// 丢弃（§6.2）
		default:
			return nil, invalid(fmt.Sprintf("%s.content[%d]: unsupported part in user message", path, j))
		}
	}
	if len(parts) == 0 {
		return out, nil
	}
	return append(out, chatOutMessage{Role: "user", Content: parts}), nil
}

func chatAssistant(m ir.Message, path string) ([]chatOutMessage, error) {
	var text strings.Builder
	var hasText bool
	var calls []chatToolCall
	for j, p := range m.Parts {
		switch v := p.(type) {
		case ir.TextPart:
			hasText = true
			text.WriteString(v.Text)
		case ir.ToolCallPart:
			args := string(v.Arguments)
			if args == "" {
				args = "{}"
			}
			tc := chatToolCall{ID: v.ID, Type: "function"}
			tc.Function.Name, tc.Function.Arguments = v.Name, &args
			calls = append(calls, tc)
		case ir.ThinkingPart:
			// 任何厂商的思考内容都不回传给 Chat 上游（§6.2）
		default:
			return nil, invalid(fmt.Sprintf("%s.content[%d]: unsupported part in assistant message", path, j))
		}
	}
	if !hasText && len(calls) == 0 {
		return nil, nil
	}
	msg := chatOutMessage{Role: "assistant", ToolCalls: calls}
	if hasText {
		msg.Content = text.String()
	}
	return []chatOutMessage{msg}, nil
}

func chatImageURL(img ir.ImagePart, path string) (string, error) {
	switch {
	case len(img.Data) > 0:
		return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data), nil
	case img.URL != "":
		return img.URL, nil
	default:
		return "", invalid(path + ": image has neither data nor url")
	}
}
