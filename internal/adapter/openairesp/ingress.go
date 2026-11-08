package openairesp

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是 OpenAI Responses 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var (
	_ protocol.Ingress    = Ingress{}
	_ protocol.ToolSource = Ingress{}
)

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// Routes 实现 IN-R01。不声明目录路由：GET /v1/models 由 openai_chat 提供（同一格式）。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/responses", Kind: protocol.RouteCall, Op: OpCreate},
		{Method: http.MethodGet, Path: "/v1/responses/{id}", Kind: protocol.RouteCall, Op: OpGet, Body: protocol.BodyNone},
		{Method: http.MethodDelete, Path: "/v1/responses/{id}", Kind: protocol.RouteCall, Op: OpDelete, Body: protocol.BodyNone},
		{Method: http.MethodPost, Path: "/v1/responses/{id}/cancel", Kind: protocol.RouteCall, Op: OpCancel, Body: protocol.BodyNone},
		{Method: http.MethodGet, Path: "/v1/responses/{id}/input_items", Kind: protocol.RouteCall, Op: OpInputItems, Body: protocol.BodyNone},
	}
}

// Claims 不认领：本协议的路由不与其他协议冲突。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 IN-R02 ~ IN-R06。
func (Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	env := &protocol.Envelope{
		Protocol: Protocol,
		Op:       rt.Op,
		Header:   r.Header,
	}
	if rt.Body == protocol.BodyBuffered {
		env.Body = body
	}

	if rt.Op != OpCreate {
		// IN-R05:
		env.Model = r.Header.Get("X-UAIG-Model")
		id := r.PathValue("id")
		params := map[string]string{"id": id}
		if q := r.URL.RawQuery; q != "" {
			params["query"] = q
		}
		env.Params = params
		env.Estimate = pricing.Usage{
			pricing.UnitInputTokens:     0,
			pricing.UnitOutputTokens:    0,
			pricing.UnitCacheReadTokens: 0,
		}
		env.Affinity = protocol.AffinityHint{
			Source: protocol.AffinityPin,
			Value:  []byte(id),
			Strict: true,
		}
		return env, nil
	}

	// responses.create: IN-L01 ~ IN-L03, IN-R02 ~ IN-R04
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}

	model, ok := jsonx.GetString(body, "model")
	if !ok || model == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "model: required")
	}
	env.Model = model

	if streamRaw, ok := jsonx.Get(body, "stream"); ok {
		var s bool
		if err := json.Unmarshal(streamRaw, &s); err != nil {
			return nil, apperr.New(apperr.CodeInvalidRequest, "stream must be a boolean")
		}
		env.Stream = s
	}

	est := pricing.Usage{
		pricing.UnitInputTokens: pricing.EstimateInputTokens(len(body)),
	}
	if _, ok := jsonx.Get(body, "max_output_tokens"); ok {
		maxTokens, ok := jsonx.GetInt64(body, "max_output_tokens")
		if !ok || maxTokens <= 0 {
			return nil, apperr.New(apperr.CodeInvalidRequest, "max_output_tokens must be a positive integer")
		}
		est[pricing.UnitOutputTokens] = maxTokens
	}
	env.Estimate = est

	if prevID, ok := jsonx.GetString(body, "previous_response_id"); ok && prevID != "" {
		env.Affinity = protocol.AffinityHint{
			Source: protocol.AffinityPin,
			Value:  []byte(prevID),
			Strict: true,
		}
	} else if cacheKey, ok := jsonx.GetString(body, "prompt_cache_key"); ok && cacheKey != "" {
		env.Affinity = protocol.AffinityHint{
			Source: protocol.AffinityUser,
			Value:  []byte(cacheKey),
		}
	} else if safetyID, ok := jsonx.GetString(body, "safety_identifier"); ok && safetyID != "" {
		env.Affinity = protocol.AffinityHint{
			Source: protocol.AffinityUser,
			Value:  []byte(safetyID),
		}
	} else if user, ok := jsonx.GetString(body, "user"); ok && user != "" {
		env.Affinity = protocol.AffinityHint{
			Source: protocol.AffinityUser,
			Value:  []byte(user),
		}
	} else {
		var (
			bInstructions []byte
			bTools        []byte
			bInput        []byte
			hasAny        bool
		)
		if raw, ok := jsonx.Get(body, "instructions"); ok {
			bInstructions = bytes.TrimSpace(raw)
			hasAny = true
		}
		if raw, ok := jsonx.Get(body, "tools"); ok {
			bTools = bytes.TrimSpace(raw)
			hasAny = true
		}
		if raw, ok := jsonx.Get(body, "input"); ok {
			trimmedInput := bytes.TrimSpace(raw)
			if len(trimmedInput) > 0 && trimmedInput[0] == '[' {
				if elem, ok := jsonx.ArrayElem(trimmedInput, 0); ok {
					bInput = bytes.TrimSpace(elem)
					hasAny = true
				}
			} else {
				bInput = trimmedInput
				hasAny = true
			}
		}
		if hasAny {
			env.Affinity = protocol.AffinityHint{
				Source: protocol.AffinityPrefix,
				Value:  bytes.Join([][]byte{bInstructions, bTools, bInput}, []byte{0x00}),
			}
		}
	}

	return env, nil
}

var errorType = map[apperr.Code]string{
	apperr.CodeInvalidRequest:    "invalid_request_error",
	apperr.CodeUnauthenticated:   "authentication_error",
	apperr.CodeInsufficientQuota: "insufficient_quota",
	apperr.CodePermissionDenied:  "permission_error",
	apperr.CodeNotFound:          "not_found_error",
	apperr.CodeConflict:          "invalid_request_error",
	apperr.CodeRequestTooLarge:   "invalid_request_error",
	apperr.CodeRateLimited:       "rate_limit_error",
}

// ErrorType 返回错误码对应的 error.type（未列出的一律为 server_error），与 openai_chat 一致。
func ErrorType(c apperr.Code) string {
	if t, ok := errorType[c]; ok {
		return t
	}
	return "server_error"
}

// WriteError 写 {"error":{"message","type","param","code"}}（与 Chat Completions 相同的格式）。
func (Ingress) WriteError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{"error": map[string]any{
		"message": err.Message, "type": ErrorType(err.Code), "param": nil, "code": string(err.Code),
	}})
}

// StreamError 实现 IN-R07：event: error，data: {"type":"error","code":<错误码>,"message":<消息>,"param":null}。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	code := ""
	msg := ""
	if err != nil {
		code = string(err.Code)
		msg = err.Message
	}
	data, _ := json.Marshal(map[string]any{
		"type":    "error",
		"code":    code,
		"message": msg,
		"param":   nil,
	})
	return sse.Event{
		Name: "error",
		Data: data,
	}
}

// WriteCatalog 不会被调用（没有目录路由）。
func (Ingress) WriteCatalog(w http.ResponseWriter, _ []protocol.ModelInfo) {
	w.WriteHeader(http.StatusNotFound)
}
