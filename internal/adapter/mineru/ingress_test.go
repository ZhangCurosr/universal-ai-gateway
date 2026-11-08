package mineru

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
	if len(routes) != 4 {
		t.Fatalf("Routes() count = %d, want 4", len(routes))
	}

	ops := map[string]protocol.Route{}
	for _, rt := range routes {
		if rt.Kind != protocol.RouteCall {
			t.Errorf("route %s %s: kind is not RouteCall", rt.Method, rt.Path)
		}
		if _, exists := ops[rt.Op]; exists {
			t.Errorf("duplicate op %s", rt.Op)
		}
		ops[rt.Op] = rt
	}

	if rt, ok := ops[OpParse]; !ok {
		t.Errorf("missing route for %s", OpParse)
	} else if rt.Method != http.MethodPost || rt.Path != "/v1/parse" || rt.Body != protocol.BodyStream {
		t.Errorf("unexpected route for %s: %+v", OpParse, rt)
	}

	if rt, ok := ops[OpTaskCreate]; !ok {
		t.Errorf("missing route for %s", OpTaskCreate)
	} else if rt.Method != http.MethodPost || rt.Path != "/v1/tasks" || rt.Body != protocol.BodyStream {
		t.Errorf("unexpected route for %s: %+v", OpTaskCreate, rt)
	}

	if rt, ok := ops[OpTaskGet]; !ok {
		t.Errorf("missing route for %s", OpTaskGet)
	} else if rt.Method != http.MethodGet || rt.Path != "/v1/tasks/{id}" || rt.Body != protocol.BodyNone {
		t.Errorf("unexpected route for %s: %+v", OpTaskGet, rt)
	}

	if rt, ok := ops[OpTaskResult]; !ok {
		t.Errorf("missing route for %s", OpTaskResult)
	} else if rt.Method != http.MethodGet || rt.Path != "/v1/tasks/{id}/result" || rt.Body != protocol.BodyNone {
		t.Errorf("unexpected route for %s: %+v", OpTaskResult, rt)
	}
}

func TestIngressClaims(t *testing.T) {
	in := Ingress{}
	req := httptest.NewRequest(http.MethodPost, "/v1/parse", nil)
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
	if len(tools) != 1 {
		t.Fatalf("Tools() count = %d, want 1", len(tools))
	}
	tl := tools[0]
	if tl.Name != ToolParse {
		t.Errorf("tool name = %q, want %q", tl.Name, ToolParse)
	}
	if !protocol.ToolNamePattern.MatchString(tl.Name) {
		t.Errorf("invalid tool name %q", tl.Name)
	}
	if tl.Description == "" {
		t.Errorf("tool %q has empty description", tl.Name)
	}
	if !tl.ReadOnly {
		t.Errorf("tool %q should be read-only", tl.Name)
	}
	if tl.Model != ModelParse {
		t.Errorf("tool model = %q, want %q", tl.Model, ModelParse)
	}

	var schema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(tl.InputSchema, &schema); err != nil || schema.Type != "object" {
		t.Errorf("invalid schema object: %v", err)
	}
}

func TestToolSource_ToolRequest(t *testing.T) {
	in := Ingress{}

	// Valid request
	req, err := in.ToolRequest(ToolParse, json.RawMessage(`{"url":"https://example.com/test.pdf"}`))
	if err != nil {
		t.Fatalf("ToolRequest() error = %v", err)
	}
	if req.Op != OpParse {
		t.Errorf("req.Op = %q, want %q", req.Op, OpParse)
	}
	if req.Query.Get("url") != "https://example.com/test.pdf" {
		t.Errorf("req.Query url = %q", req.Query.Get("url"))
	}
	if !strings.Contains(string(req.Body), "https://example.com/test.pdf") {
		t.Errorf("req.Body = %s", string(req.Body))
	}

	// Missing url
	_, err = in.ToolRequest(ToolParse, json.RawMessage(`{}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("missing url error = %v, want invalid_request", err)
	}

	// Empty url
	_, err = in.ToolRequest(ToolParse, json.RawMessage(`{"url":"  "}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("empty url error = %v, want invalid_request", err)
	}

	// Unknown field (disallowed)
	_, err = in.ToolRequest(ToolParse, json.RawMessage(`{"url":"https://example.com/doc.pdf","extra":"val"}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("extra field error = %v, want invalid_request", err)
	}

	// Unknown tool
	_, err = in.ToolRequest("unknown_tool", json.RawMessage(`{}`))
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("unknown tool error = %v, want invalid_request", err)
	}
}

func TestToolSource_ToolResult(t *testing.T) {
	in := Ingress{}

	// 200 OK with JSON markdown
	jsonBody := []byte(`{"markdown":"# Title\nExtracted content","pages":3}`)
	res, err := in.ToolResult(ToolParse, http.StatusOK, nil, jsonBody)
	if err != nil {
		t.Fatalf("ToolResult error = %v", err)
	}
	if res.IsError {
		t.Errorf("res.IsError = true, want false")
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "# Title") {
		t.Errorf("unexpected content: %+v", res.Content)
	}
	if len(res.Structured) == 0 {
		t.Error("res.Structured is empty")
	}

	// 200 OK with raw text
	rawBody := []byte("# Raw Markdown\nDirect content")
	res, err = in.ToolResult(ToolParse, http.StatusOK, nil, rawBody)
	if err != nil {
		t.Fatalf("ToolResult raw error = %v", err)
	}
	if res.IsError {
		t.Errorf("res.IsError = true, want false")
	}
	if len(res.Content) == 0 || res.Content[0].Text != string(rawBody) {
		t.Errorf("unexpected content: %+v", res.Content)
	}

	// Non-2xx response: extracts error message
	errBody := []byte(`{"msg":"file format not supported"}`)
	res, err = in.ToolResult(ToolParse, http.StatusBadRequest, nil, errBody)
	if err != nil {
		t.Fatalf("ToolResult error = %v", err)
	}
	if !res.IsError {
		t.Errorf("res.IsError = false, want true")
	}
	if !strings.Contains(res.Content[0].Text, "file format not supported") {
		t.Errorf("unexpected error text: %s", res.Content[0].Text)
	}

	// Unknown tool
	_, err = in.ToolResult("unknown", http.StatusOK, nil, rawBody)
	if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
		t.Errorf("unknown tool error = %v, want invalid_request", err)
	}
}
