package tavily

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestIngressRoutes(t *testing.T) {
	in := Ingress{}
	routes := in.Routes()
	if len(routes) != 3 {
		t.Fatalf("Routes() count = %d, want 3", len(routes))
	}
	ops := map[string]bool{}
	for _, rt := range routes {
		if rt.Method != http.MethodPost {
			t.Errorf("route %s %s: method is not POST", rt.Method, rt.Path)
		}
		if rt.Body != protocol.BodyBuffered {
			t.Errorf("route %s %s: body mode is not BodyBuffered", rt.Method, rt.Path)
		}
		if rt.Kind != protocol.RouteCall {
			t.Errorf("route %s %s: kind is not RouteCall", rt.Method, rt.Path)
		}
		if ops[rt.Op] {
			t.Errorf("duplicate op %s", rt.Op)
		}
		ops[rt.Op] = true
	}
	if !ops[OpSearch] || !ops[OpExtract] || !ops[OpCrawl] {
		t.Errorf("missing expected ops in routes: %v", ops)
	}
}

func TestIngressClaims(t *testing.T) {
	in := Ingress{}
	req := httptest.NewRequest(http.MethodPost, "/search", nil)
	if in.Claims(req) {
		t.Errorf("Claims() = true, want false")
	}
}

func TestIngressWriteError(t *testing.T) {
	in := Ingress{}
	rec := httptest.NewRecorder()
	err := apperr.New(apperr.CodeRateLimited, "rate limited").WithRetryAfter(5 * time.Second)
	in.WriteError(rec, "req_123", err)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if rec.Header().Get("Retry-After") != "5" {
		t.Errorf("Retry-After = %q, want 5", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("body missing error message: %s", rec.Body.String())
	}
}

func TestIngressStreamError(t *testing.T) {
	in := Ingress{}
	ev := in.StreamError(apperr.New(apperr.CodeInternal, "internal server failure"))
	if ev.Name != "error" {
		t.Errorf("event name = %q, want 'error'", ev.Name)
	}
	if !strings.Contains(string(ev.Data), "internal server failure") {
		t.Errorf("event data missing message: %s", string(ev.Data))
	}
}

func TestIngressWriteCatalog(t *testing.T) {
	in := Ingress{}
	rec := httptest.NewRecorder()
	in.WriteCatalog(rec, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestToolSource_Tools(t *testing.T) {
	in := Ingress{}
	tools := in.Tools()
	if len(tools) != 2 {
		t.Fatalf("Tools() count = %d, want 2", len(tools))
	}
	names := map[string]protocol.Tool{}
	for _, tl := range tools {
		if !protocol.ToolNamePattern.MatchString(tl.Name) {
			t.Errorf("invalid tool name %q", tl.Name)
		}
		if tl.Description == "" {
			t.Errorf("tool %q has empty description", tl.Name)
		}
		if !tl.ReadOnly {
			t.Errorf("tool %q should be read-only", tl.Name)
		}
		var schema struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(tl.InputSchema, &schema); err != nil || schema.Type != "object" {
			t.Errorf("tool %q: invalid schema object: %v", tl.Name, err)
		}
		names[tl.Name] = tl
	}
	if _, ok := names[ToolSearch]; !ok {
		t.Errorf("missing tool %q", ToolSearch)
	}
	if _, ok := names[ToolExtract]; !ok {
		t.Errorf("missing tool %q", ToolExtract)
	}
}

func TestToolSource_ToolRequest(t *testing.T) {
	in := Ingress{}

	// tavily_search: valid
	req, err := in.ToolRequest(ToolSearch, json.RawMessage(`{"query":"golang concurrency","search_depth":"advanced","max_results":10}`))
	if err != nil {
		t.Fatalf("ToolRequest(tavily_search) error = %v", err)
	}
	if req.Op != OpSearch {
		t.Errorf("req.Op = %q, want %q", req.Op, OpSearch)
	}
	if !strings.Contains(string(req.Body), "golang concurrency") {
		t.Errorf("req.Body = %s", string(req.Body))
	}

	// tavily_search: missing query
	_, err = in.ToolRequest(ToolSearch, json.RawMessage(`{"search_depth":"advanced"}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("missing query error = %v, want invalid_request", err)
	}

	// tavily_search: invalid search_depth
	_, err = in.ToolRequest(ToolSearch, json.RawMessage(`{"query":"hi","search_depth":"deep"}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("invalid search_depth error = %v, want invalid_request", err)
	}

	// tavily_search: invalid max_results
	_, err = in.ToolRequest(ToolSearch, json.RawMessage(`{"query":"hi","max_results":0}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("invalid max_results error = %v, want invalid_request", err)
	}

	// tavily_extract: valid
	req, err = in.ToolRequest(ToolExtract, json.RawMessage(`{"urls":["https://go.dev"]}`))
	if err != nil {
		t.Fatalf("ToolRequest(tavily_extract) error = %v", err)
	}
	if req.Op != OpExtract {
		t.Errorf("req.Op = %q, want %q", req.Op, OpExtract)
	}
	if !strings.Contains(string(req.Body), "https://go.dev") {
		t.Errorf("req.Body = %s", string(req.Body))
	}

	// tavily_extract: empty urls
	_, err = in.ToolRequest(ToolExtract, json.RawMessage(`{"urls":[]}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("empty urls error = %v, want invalid_request", err)
	}

	// tavily_extract: empty url element
	_, err = in.ToolRequest(ToolExtract, json.RawMessage(`{"urls":[""]}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("empty url element error = %v, want invalid_request", err)
	}

	// unknown tool
	_, err = in.ToolRequest("unknown", json.RawMessage(`{}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("unknown tool error = %v, want invalid_request", err)
	}
}

func TestToolSource_ToolResult(t *testing.T) {
	in := Ingress{}

	// tavily_search: 200 success with results & answer
	searchResp := []byte(`{
		"answer": "Go is an open-source programming language.",
		"results": [
			{"title": "The Go Programming Language", "url": "https://go.dev", "content": "Build simple, secure, scalable systems."}
		]
	}`)
	res, err := in.ToolResult(ToolSearch, http.StatusOK, nil, searchResp)
	if err != nil {
		t.Fatalf("ToolResult(tavily_search) error = %v", err)
	}
	if res.IsError {
		t.Errorf("res.IsError = true, want false")
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "https://go.dev") {
		t.Errorf("unexpected content: %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].Text, "Answer: Go is an open-source") {
		t.Errorf("content missing answer: %s", res.Content[0].Text)
	}
	if len(res.Structured) == 0 {
		t.Error("res.Structured is empty")
	}

	// tavily_search: 200 with no results
	res, err = in.ToolResult(ToolSearch, http.StatusOK, nil, []byte(`{"results":[]}`))
	if err != nil {
		t.Fatalf("ToolResult empty results error = %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "No results found") {
		t.Errorf("expected 'No results found', got: %s", res.Content[0].Text)
	}

	// tavily_extract: 200 success with results & failed_results
	extractResp := []byte(`{
		"results": [
			{"url": "https://go.dev", "title": "Go", "raw_content": "Go package docs."}
		],
		"failed_results": [
			{"url": "https://bad.example.com", "error": "404 Not Found"}
		]
	}`)
	res, err = in.ToolResult(ToolExtract, http.StatusOK, nil, extractResp)
	if err != nil {
		t.Fatalf("ToolResult(tavily_extract) error = %v", err)
	}
	if res.IsError {
		t.Errorf("res.IsError = true, want false")
	}
	if !strings.Contains(res.Content[0].Text, "Go package docs.") {
		t.Errorf("missing extracted text: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "Failed URLs:") {
		t.Errorf("missing failed URLs: %s", res.Content[0].Text)
	}

	// tavily_extract: 200 with no content
	res, err = in.ToolResult(ToolExtract, http.StatusOK, nil, []byte(`{"results":[],"failed_results":[]}`))
	if err != nil {
		t.Fatalf("ToolResult empty extract error = %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "No content extracted") {
		t.Errorf("expected 'No content extracted', got: %s", res.Content[0].Text)
	}

	// Non-2xx response: extracts error message
	errBody := []byte(`{"detail":"invalid API key provided"}`)
	res, err = in.ToolResult(ToolSearch, http.StatusUnauthorized, nil, errBody)
	if err != nil {
		t.Fatalf("ToolResult(401) error = %v", err)
	}
	if !res.IsError {
		t.Errorf("res.IsError = false, want true")
	}
	if !strings.Contains(res.Content[0].Text, "invalid API key provided") {
		t.Errorf("unexpected error text: %s", res.Content[0].Text)
	}

	// Invalid JSON on 200 returns error
	_, err = in.ToolResult(ToolSearch, http.StatusOK, nil, []byte(`not json`))
	if err == nil {
		t.Error("expected unmarshal error on invalid JSON, got nil")
	}
}
