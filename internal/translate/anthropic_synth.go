package translate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
)

// DefaultMaxTokens 是 IR 未指定 MaxTokens 时发给 Anthropic 的 max_tokens（Anthropic 必填，spec/10 §4）。
const DefaultMaxTokens = 8192

// continuePlaceholder 是首条消息不是 user 时插入的占位消息文本（spec/10 §4）。
const continuePlaceholder = "(continue)"

// emptyObjectSchema 用于没有声明参数的工具：OpenAI 允许省略 parameters（表示无参数），
// Anthropic 的 input_schema 必填，二者语义相同。
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// anthropicRequest 的字段顺序即输出顺序（便于黄金文件比较与排查）。
type anthropicRequest struct {
	Model         string               `json:"model"`
	System        []anthropicBlock     `json:"system,omitempty"`
	Messages      []anthropicMessage   `json:"messages"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	MaxTokens     int64                `json:"max_tokens"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Metadata      *anthropicMetadata   `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    ir.Role          `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock 是一个内容块；按 Type 只填对应字段。
type anthropicBlock struct {
	Type         string                `json:"type"`
	Text         *string               `json:"text,omitempty"`
	Source       *anthropicImageSource `json:"source,omitempty"`
	ID           string                `json:"id,omitempty"`
	Name         string                `json:"name,omitempty"`
	Input        json.RawMessage       `json:"input,omitempty"`
	ToolUseID    string                `json:"tool_use_id,omitempty"`
	Content      []anthropicBlock      `json:"content,omitempty"`
	IsError      bool                  `json:"is_error,omitempty"`
	Thinking     *string               `json:"thinking,omitempty"`
	Signature    string                `json:"signature,omitempty"`
	Data         string                `json:"data,omitempty"`
	CacheControl *anthropicCache       `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicCache struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type anthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *anthropicCache `json:"cache_control,omitempty"`
}

type anthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool
	Name string `json:"name,omitempty"`
}

type anthropicMetadata struct {
	UserID string `json:"user_id"`
}

// synthesizeAnthropic 把 IR 请求合成为 Anthropic Messages 请求体（spec/10 §4，T21）。
// model 取 req.Model（调用方先把它设为上游服务名）。无法表达的内容返回 CodeInvalidRequest。
func synthesizeAnthropic(req *ir.Request, log *slog.Logger) ([]byte, error) {
	if log == nil {
		log = obs.Discard()
	}
	out := anthropicRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}
	if req.User != "" {
		out.Metadata = &anthropicMetadata{UserID: req.User}
	}
	for _, s := range req.System {
		out.System = append(out.System, textBlock(s))
	}

	msgs, err := anthropicMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if len(msgs) > 0 && msgs[0].Role != ir.RoleUser {
		log.Debug("translate: first message is not from user; inserting placeholder", "role", msgs[0].Role)
		text := continuePlaceholder
		msgs = append([]anthropicMessage{{Role: ir.RoleUser, Content: []anthropicBlock{{Type: "text", Text: &text}}}}, msgs...)
	}
	if len(msgs) == 0 {
		return nil, invalid("messages: nothing left to send after translation")
	}
	out.Messages = msgs

	// tool_choice=none → 不发送 tools 与 tool_choice（§4）
	if req.ToolChoice == nil || req.ToolChoice.Mode != "none" {
		for _, t := range req.Tools {
			schema := t.InputSchema
			if len(bytes.TrimSpace(schema)) == 0 {
				schema = emptyObjectSchema
			}
			out.Tools = append(out.Tools, anthropicTool{
				Name: t.Name, Description: t.Description, InputSchema: schema, CacheControl: cacheOf(t.CacheControl),
			})
		}
		if tc := req.ToolChoice; tc != nil {
			switch tc.Mode {
			case "auto", "any":
				out.ToolChoice = &anthropicToolChoice{Type: tc.Mode}
			case "tool":
				if tc.Name == "" {
					return nil, invalid("tool_choice: tool name required")
				}
				out.ToolChoice = &anthropicToolChoice{Type: "tool", Name: tc.Name}
			default:
				return nil, invalid("tool_choice mode " + tc.Mode + " is not supported")
			}
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	return b, nil
}

// anthropicMessages 转换消息并合并相邻同角色消息（Anthropic 要求 user/assistant 交替）。
// 转换后为空的消息（例如只含其他厂商思考块的 assistant 消息）被跳过，再参与合并。
func anthropicMessages(in []ir.Message) ([]anthropicMessage, error) {
	var out []anthropicMessage
	for i, m := range in {
		if m.Role != ir.RoleUser && m.Role != ir.RoleAssistant {
			return nil, invalid(fmt.Sprintf("messages[%d]: role %s is not supported", i, m.Role))
		}
		var blocks []anthropicBlock
		for j, p := range m.Parts {
			b, keep, err := anthropicPart(p, fmt.Sprintf("messages[%d].parts[%d]", i, j))
			if err != nil {
				return nil, err
			}
			if keep {
				blocks = append(blocks, b)
			}
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, anthropicMessage{Role: m.Role, Content: blocks})
	}
	return out, nil
}

// anthropicPart 转换一个内容块；keep=false 表示按规则丢弃（其他厂商的思考块）。
func anthropicPart(p ir.Part, path string) (anthropicBlock, bool, error) {
	switch v := p.(type) {
	case ir.TextPart:
		return textBlock(v), true, nil
	case ir.ImagePart:
		b, err := imageBlock(v, path)
		return b, err == nil, err
	case ir.ToolCallPart:
		args := bytes.TrimSpace(v.Arguments)
		if len(args) == 0 {
			args = []byte("{}")
		}
		if !json.Valid(args) || args[0] != '{' {
			return anthropicBlock{}, false, invalid(path + ": tool call arguments must be a JSON object")
		}
		return anthropicBlock{Type: "tool_use", ID: v.ID, Name: v.Name, Input: json.RawMessage(args)}, true, nil
	case ir.ToolResultPart:
		b := anthropicBlock{Type: "tool_result", ToolUseID: v.ToolCallID, IsError: v.IsError}
		for k, c := range v.Content {
			cp := fmt.Sprintf("%s.content[%d]", path, k)
			switch cv := c.(type) {
			case ir.TextPart:
				b.Content = append(b.Content, textBlock(cv))
			case ir.ImagePart:
				img, err := imageBlock(cv, cp)
				if err != nil {
					return anthropicBlock{}, false, err
				}
				b.Content = append(b.Content, img)
			default:
				return anthropicBlock{}, false, invalid(cp + ": tool results may only contain text and images")
			}
		}
		return b, true, nil
	case ir.ThinkingPart:
		// 思考签名只能回传给签发它的厂商（§1 第 3 条）：其他厂商一律丢弃
		if v.Vendor != ir.VendorAnthropic {
			return anthropicBlock{}, false, nil
		}
		if v.Redacted {
			return anthropicBlock{Type: "redacted_thinking", Data: v.RedactedData}, true, nil
		}
		text := v.Text
		return anthropicBlock{Type: "thinking", Thinking: &text, Signature: v.Signature}, true, nil
	default:
		return anthropicBlock{}, false, invalid(fmt.Sprintf("%s: unsupported content part %T", path, p))
	}
}

func textBlock(t ir.TextPart) anthropicBlock {
	text := t.Text
	return anthropicBlock{Type: "text", Text: &text, CacheControl: cacheOf(t.CacheControl)}
}

func imageBlock(img ir.ImagePart, path string) (anthropicBlock, error) {
	switch {
	case len(img.Data) > 0:
		if img.MediaType == "" {
			return anthropicBlock{}, invalid(path + ": image media type required")
		}
		return anthropicBlock{Type: "image", Source: &anthropicImageSource{
			Type: "base64", MediaType: img.MediaType, Data: base64.StdEncoding.EncodeToString(img.Data),
		}}, nil
	case img.URL != "":
		return anthropicBlock{Type: "image", Source: &anthropicImageSource{Type: "url", URL: img.URL}}, nil
	default:
		return anthropicBlock{}, invalid(path + ": image has neither data nor url")
	}
}

func cacheOf(c *ir.CacheControl) *anthropicCache {
	if c == nil {
		return nil
	}
	return &anthropicCache{Type: c.Type, TTL: c.TTL}
}
