package tavily

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是 Tavily 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var (
	_ protocol.Ingress    = Ingress{}
	_ protocol.ToolSource = Ingress{}
)

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// Routes 声明 Tavily 的 3 个端点（spec/08 §8.1）。无目录路由。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/search", Kind: protocol.RouteCall, Op: OpSearch, Body: protocol.BodyBuffered},
		{Method: http.MethodPost, Path: "/extract", Kind: protocol.RouteCall, Op: OpExtract, Body: protocol.BodyBuffered},
		{Method: http.MethodPost, Path: "/crawl", Kind: protocol.RouteCall, Op: OpCrawl, Body: protocol.BodyBuffered},
	}
}

// Claims 永远返回 false（无冲突端点）。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 IN-G01 ~ IN-G06 与 spec/08 §8.1。
func (Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}

	model := r.Header.Get("X-UAIG-Model")
	if model == "" {
		switch rt.Op {
		case OpSearch:
			model = ModelSearch
		case OpExtract:
			model = ModelExtract
		case OpCrawl:
			model = ModelCrawl
		default:
			return nil, apperr.New(apperr.CodeInvalidRequest, "unknown operation: "+rt.Op)
		}
	}

	var est pricing.Usage
	if rt.Op == OpSearch {
		q, ok := jsonx.GetString(body, "query")
		if !ok || strings.TrimSpace(q) == "" {
			return nil, apperr.New(apperr.CodeInvalidRequest, "query: required")
		}
		credits := int64(1)
		if depth, ok := jsonx.GetString(body, "search_depth"); ok && depth == "advanced" {
			credits = 2
		}
		est = pricing.Usage{UnitCredits: credits}
	}

	return &protocol.Envelope{
		Protocol: Protocol,
		Op:       rt.Op,
		Model:    model,
		Stream:   false,
		Estimate: est,
		Body:     body,
		Header:   r.Header,
	}, nil
}

// WriteError 写 Tavily 风格的 JSON 错误响应（spec/08 §2.5）。
func (Ingress) WriteError(w http.ResponseWriter, _ string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{
		"detail": err.Message,
		"error":  err.Message,
	})
}

// StreamError 返回流错误事件（spec/08 §2.6）。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	data, _ := json.Marshal(map[string]any{"detail": err.Message, "error": err.Message})
	return sse.Event{Name: "error", Data: data}
}

// WriteCatalog 实现没有目录路由的 404 响应（spec/08 §2.8）。
func (Ingress) WriteCatalog(w http.ResponseWriter, _ []protocol.ModelInfo) {
	apperr.WriteJSON(w, apperr.New(apperr.CodeNotFound, "catalog not supported for tavily"), map[string]any{
		"detail": "catalog not supported",
	})
}

// ---------------------------------------------------------------- ToolSource (spec/14 §1)

const (
	ToolSearch  = "tavily_search"
	ToolExtract = "tavily_extract"
)

var (
	searchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "The search query string to execute."
    },
    "search_depth": {
      "type": "string",
      "enum": ["basic", "advanced"],
      "description": "The depth of search: 'basic' (faster, 1 credit) or 'advanced' (more thorough, 2 credits)."
    },
    "max_results": {
      "type": "integer",
      "minimum": 1,
      "description": "Maximum number of search results to return."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)

	extractSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "urls": {
      "type": "array",
      "items": {
        "type": "string"
      },
      "minItems": 1,
      "description": "List of web URLs to extract text content from."
    }
  },
  "required": ["urls"],
  "additionalProperties": false
}`)
)

// Tools 返回 Tavily 提供的工具（TS-01 ~ TS-03）。
func (Ingress) Tools() []protocol.Tool {
	return []protocol.Tool{
		{
			Name:        ToolSearch,
			Title:       "Tavily Web Search",
			Description: "Search the web using Tavily and return relevant results with titles, URLs, and snippets. Use this tool when you need up-to-date web information or facts.",
			InputSchema: searchSchema,
			Model:       ModelSearch,
			ReadOnly:    true,
		},
		{
			Name:        ToolExtract,
			Title:       "Tavily Web Content Extraction",
			Description: "Extract web page content from one or more URLs using Tavily. Returns the extracted text for each URL. Use this tool when you have specific URLs to inspect.",
			InputSchema: extractSchema,
			Model:       ModelExtract,
			ReadOnly:    true,
		},
	}
}

// ToolRequest 实现 TS-03、TS-04：参数转换为本协议调用端点的请求。
func (Ingress) ToolRequest(name string, args json.RawMessage) (*protocol.ToolRequest, error) {
	switch name {
	case ToolSearch:
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		var input struct {
			Query       string  `json:"query"`
			SearchDepth *string `json:"search_depth,omitempty"`
			MaxResults  *int    `json:"max_results,omitempty"`
		}
		if err := dec.Decode(&input); err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_search: "+err.Error())
		}
		if strings.TrimSpace(input.Query) == "" {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_search: query is required")
		}
		if input.SearchDepth != nil && *input.SearchDepth != "basic" && *input.SearchDepth != "advanced" {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_search: search_depth must be 'basic' or 'advanced'")
		}
		if input.MaxResults != nil && *input.MaxResults < 1 {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_search: max_results must be >= 1")
		}
		body, err := json.Marshal(input)
		if err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_search: "+err.Error())
		}
		return &protocol.ToolRequest{Op: OpSearch, Body: body}, nil

	case ToolExtract:
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		var input struct {
			URLs []string `json:"urls"`
		}
		if err := dec.Decode(&input); err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_extract: "+err.Error())
		}
		if len(input.URLs) == 0 {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_extract: urls is required and must not be empty")
		}
		for i, u := range input.URLs {
			if strings.TrimSpace(u) == "" {
				return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("tavily_extract: urls[%d] must not be empty", i))
			}
		}
		body, err := json.Marshal(input)
		if err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "tavily_extract: "+err.Error())
		}
		return &protocol.ToolRequest{Op: OpExtract, Body: body}, nil

	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool: "+name)
	}
}

// ToolResult 实现 TS-05、TS-06：端点响应转换为工具结果。
func (Ingress) ToolResult(name string, status int, _ http.Header, body []byte) (*protocol.ToolResult, error) {
	if status < 200 || status >= 300 {
		msg := extractErrorMessage(body)
		return protocol.TextResult(msg, true), nil
	}

	switch name {
	case ToolSearch:
		var resp struct {
			Answer  string `json:"answer"`
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}

		var sb strings.Builder
		if resp.Answer != "" {
			sb.WriteString("Answer: ")
			sb.WriteString(resp.Answer)
			sb.WriteString("\n\n")
		}
		if len(resp.Results) == 0 {
			sb.WriteString("No results found.")
		} else {
			for i, r := range resp.Results {
				if i > 0 {
					sb.WriteString("\n\n")
				}
				fmt.Fprintf(&sb, "%d. Title: %s\n   URL: %s\n   Snippet: %s", i+1, r.Title, r.URL, r.Content)
			}
		}
		return &protocol.ToolResult{
			Content:    []protocol.ToolContent{{Type: "text", Text: sb.String()}},
			Structured: json.RawMessage(body),
			IsError:    false,
		}, nil

	case ToolExtract:
		var resp struct {
			Results []struct {
				URL        string `json:"url"`
				Title      string `json:"title"`
				RawContent string `json:"raw_content"`
				Content    string `json:"content"`
			} `json:"results"`
			FailedResults []struct {
				URL   string `json:"url"`
				Error string `json:"error"`
			} `json:"failed_results"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}

		var sb strings.Builder
		if len(resp.Results) == 0 && len(resp.FailedResults) == 0 {
			sb.WriteString("No content extracted.")
		} else {
			for i, r := range resp.Results {
				if i > 0 {
					sb.WriteString("\n\n")
				}
				text := r.RawContent
				if text == "" {
					text = r.Content
				}
				if r.Title != "" {
					fmt.Fprintf(&sb, "URL: %s\nTitle: %s\nContent: %s", r.URL, r.Title, text)
				} else {
					fmt.Fprintf(&sb, "URL: %s\nContent: %s", r.URL, text)
				}
			}
			if len(resp.FailedResults) > 0 {
				if len(resp.Results) > 0 {
					sb.WriteString("\n\n")
				}
				sb.WriteString("Failed URLs:")
				for _, f := range resp.FailedResults {
					fmt.Fprintf(&sb, "\n- %s: %s", f.URL, f.Error)
				}
			}
		}
		return &protocol.ToolResult{
			Content:    []protocol.ToolContent{{Type: "text", Text: sb.String()}},
			Structured: json.RawMessage(body),
			IsError:    false,
		}, nil

	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, "unknown tool: "+name)
	}
}

// extractErrorMessage 提取上游错误响应中的消息（TS-05）。
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return "upstream error"
	}
	if s, ok := jsonx.GetString(body, "detail"); ok && s != "" {
		return s
	}
	if s, ok := jsonx.GetString(body, "detail", "error"); ok && s != "" {
		return s
	}
	if s, ok := jsonx.GetString(body, "error"); ok && s != "" {
		return s
	}
	if s, ok := jsonx.GetString(body, "error", "message"); ok && s != "" {
		return s
	}
	if s, ok := jsonx.GetString(body, "message"); ok && s != "" {
		return s
	}
	return string(body)
}
