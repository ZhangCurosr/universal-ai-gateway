package mineru

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是 MinerU 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var (
	_ protocol.Ingress    = Ingress{}
	_ protocol.ToolSource = Ingress{}
)

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// Routes 声明 MinerU 的 4 个端点（spec/08 §8.2）。无目录路由。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{
			Method:       http.MethodPost,
			Path:         "/v1/parse",
			Kind:         protocol.RouteCall,
			Op:           OpParse,
			Body:         protocol.BodyStream,
			MaxBodyBytes: 200 << 20, // 200 MiB 单文件上限
		},
		{
			Method:       http.MethodPost,
			Path:         "/v1/tasks",
			Kind:         protocol.RouteCall,
			Op:           OpTaskCreate,
			Body:         protocol.BodyStream,
			MaxBodyBytes: 200 << 20,
		},
		{
			Method: http.MethodGet,
			Path:   "/v1/tasks/{id}",
			Kind:   protocol.RouteCall,
			Op:     OpTaskGet,
			Body:   protocol.BodyNone,
		},
		{
			Method: http.MethodGet,
			Path:   "/v1/tasks/{id}/result",
			Kind:   protocol.RouteCall,
			Op:     OpTaskResult,
			Body:   protocol.BodyNone,
		},
	}
}

// Claims 永远返回 false（无冲突端点）。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 IN-G01 ~ IN-G07 与 spec/08 §8.2。
func (Ingress) Parse(r *http.Request, rt protocol.Route, _ []byte) (*protocol.Envelope, error) {
	switch rt.Op {
	case OpParse, OpTaskCreate:
		model := r.Header.Get("X-UAIG-Model")
		if model == "" {
			model = ModelParse
		}

		est := pricing.Usage{
			UnitRequests: 1,
		}

		pageParam := r.URL.Query().Get("pages")
		if pageParam == "" {
			pageParam = r.URL.Query().Get("max_pages")
		}
		if pageParam != "" {
			n, err := strconv.ParseInt(pageParam, 10, 64)
			if err != nil || n <= 0 {
				return nil, apperr.New(apperr.CodeInvalidRequest, "pages must be a positive integer")
			}
			est[UnitPages] = n
		}

		params := map[string]string{}
		if u := r.URL.Query().Get("url"); u != "" {
			params["url"] = u
		}
		if q := r.URL.RawQuery; q != "" {
			params["query"] = q
		}

		return &protocol.Envelope{
			Protocol: Protocol,
			Op:       rt.Op,
			Model:    model,
			Stream:   false,
			Estimate: est,
			Params:   params,
			Header:   r.Header,
		}, nil

	case OpTaskGet, OpTaskResult:
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			return nil, apperr.New(apperr.CodeInvalidRequest, "id: required")
		}

		model := r.Header.Get("X-UAIG-Model")
		params := map[string]string{"id": id}
		if q := r.URL.RawQuery; q != "" {
			params["query"] = q
		}

		// spec/08 §8.2: 查询与下载的 Estimate 写 {pages: 0, requests: 0}，严格亲和
		est := pricing.Usage{
			UnitPages:    0,
			UnitRequests: 0,
		}
		affinity := protocol.AffinityHint{
			Source: protocol.AffinityPin,
			Value:  []byte(id),
			Strict: true,
		}

		return &protocol.Envelope{
			Protocol: Protocol,
			Op:       rt.Op,
			Model:    model,
			Stream:   false,
			Estimate: est,
			Affinity: affinity,
			Params:   params,
			Header:   r.Header,
		}, nil

	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown operation: "+rt.Op)
	}
}

// WriteError 写 MinerU 风格的 JSON 错误响应（spec/08 §2.5）。
func (Ingress) WriteError(w http.ResponseWriter, _ string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{
		"code":  err.Code.HTTPStatus(),
		"msg":   err.Message,
		"error": err.Message,
	})
}

// StreamError 返回流错误事件（spec/08 §2.6）。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	data, _ := json.Marshal(map[string]any{
		"code":  err.Code.HTTPStatus(),
		"msg":   err.Message,
		"error": err.Message,
	})
	return sse.Event{Name: "error", Data: data}
}

// WriteCatalog 实现没有目录路由的 404 响应（spec/08 §2.8）。
func (Ingress) WriteCatalog(w http.ResponseWriter, _ []protocol.ModelInfo) {
	apperr.WriteJSON(w, apperr.New(apperr.CodeNotFound, "catalog not supported for mineru"), map[string]any{
		"msg": "catalog not supported",
	})
}

// ---------------------------------------------------------------- ToolSource (spec/14 §1)

const ToolParse = "mineru_parse"

var parseSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The URL of the document file (PDF, DOCX, PPTX, etc.) to parse into Markdown."
    }
  },
  "required": ["url"],
  "additionalProperties": false
}`)

// Tools 返回 MinerU 提供的工具（TS-01 ~ TS-03）。
func (Ingress) Tools() []protocol.Tool {
	return []protocol.Tool{
		{
			Name:        ToolParse,
			Title:       "MinerU Document Parser",
			Description: "Parse a document (PDF, DOCX, PPTX, etc.) from a URL into Markdown text using MinerU. Returns the extracted Markdown content.",
			InputSchema: parseSchema,
			Model:       ModelParse,
			ReadOnly:    true,
		},
	}
}

// ToolRequest 实现 TS-03、TS-04：参数转换为本协议调用端点的请求。
func (Ingress) ToolRequest(name string, args json.RawMessage) (*protocol.ToolRequest, error) {
	if name != ToolParse {
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool: "+name)
	}

	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	var input struct {
		URL string `json:"url"`
	}
	if err := dec.Decode(&input); err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "mineru_parse: "+err.Error())
	}
	if strings.TrimSpace(input.URL) == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "mineru_parse: url is required")
	}

	body, err := json.Marshal(input)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "mineru_parse: "+err.Error())
	}

	return &protocol.ToolRequest{
		Op:    OpParse,
		Query: url.Values{"url": []string{input.URL}},
		Body:  body,
	}, nil
}

// ToolResult 实现 TS-05、TS-06：端点响应转换为工具结果。
func (Ingress) ToolResult(name string, status int, _ http.Header, body []byte) (*protocol.ToolResult, error) {
	if status < 200 || status >= 300 {
		msg := extractErrorMessage(body)
		return protocol.TextResult(msg, true), nil
	}

	if name != ToolParse {
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool: "+name)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		md := extractMarkdown(trimmed)
		if md != "" {
			return &protocol.ToolResult{
				Content:    []protocol.ToolContent{{Type: "text", Text: md}},
				Structured: json.RawMessage(trimmed),
				IsError:    false,
			}, nil
		}
	}

	// 响应为原始 Markdown 或文本
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(body)}},
		IsError: false,
	}, nil
}

// extractMarkdown 从 JSON 中尝试提取 Markdown 文本。
func extractMarkdown(body []byte) string {
	paths := [][]string{
		{"markdown"},
		{"content"},
		{"text"},
		{"data", "markdown"},
		{"data", "content"},
		{"result", "markdown"},
	}
	for _, p := range paths {
		if s, ok := jsonx.GetString(body, p...); ok && s != "" {
			return s
		}
	}
	return ""
}

// extractErrorMessage 提取上游错误响应中的消息（TS-05）。
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return "upstream error"
	}
	paths := [][]string{
		{"detail"},
		{"detail", "error"},
		{"error"},
		{"error", "message"},
		{"message"},
		{"msg"},
		{"err_msg"},
		{"data", "error"},
		{"data", "err_msg"},
	}
	for _, p := range paths {
		if s, ok := jsonx.GetString(body, p...); ok && s != "" {
			return s
		}
	}
	return string(body)
}
