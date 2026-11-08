package openai

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

// Ingress 是 OpenAI Chat 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var _ protocol.Ingress = Ingress{}

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// OpChatCompletions 是 Chat Completions 调用的操作名。
const OpChatCompletions = "chat.completions"

// Routes 声明 POST /v1/chat/completions 与 GET /v1/models（IN-O01）。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/chat/completions", Kind: protocol.RouteCall, Op: OpChatCompletions},
		{Method: http.MethodGet, Path: "/v1/models", Kind: protocol.RouteCatalog},
	}
}

// Claims 永不主动认领：OpenAI 是兜底协议，冲突路由上没人认领时自动归它（IN-O02）。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 IN-G01 ~ IN-G06、IN-L01 ~ IN-L04 与 AFH-01 ~ AFH-04（spec/08 §2）。
// 输出上限：max_completion_tokens 优先于 max_tokens。
func (Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	top, err := jsonx.Object(body)
	if err != nil {
		return nil, invalid("request body must be a JSON object") // IN-L01
	}
	var model string
	if json.Unmarshal(top["model"], &model) != nil || model == "" {
		return nil, invalid("model: required") // IN-L02
	}
	stream := false
	if raw := top["stream"]; !jsonx.IsNull(raw) && json.Unmarshal(raw, &stream) != nil {
		return nil, invalid("stream: must be a boolean") // IN-L03
	}
	est := pricing.Usage{pricing.UnitInputTokens: pricing.EstimateInputTokens(len(body))}
	for _, field := range []string{"max_completion_tokens", "max_tokens"} { // IN-L04
		raw := top[field]
		if jsonx.IsNull(raw) {
			continue
		}
		var n int64
		if json.Unmarshal(raw, &n) != nil || n <= 0 {
			return nil, invalid(field + ": must be a positive integer")
		}
		est[pricing.UnitOutputTokens] = n
		break
	}
	return &protocol.Envelope{
		Protocol: Protocol,
		Op:       rt.Op,
		Model:    model,
		Stream:   stream,
		Estimate: est,
		Affinity: affinityHint(top),
		Body:     body,
		Header:   r.Header,
	}, nil
}

// affinityHint 实现 AFH-01 ~ AFH-04：prompt_cache_key > user > 前缀（tools + messages[0] + messages[1]）。
func affinityHint(top map[string]json.RawMessage) protocol.AffinityHint {
	for _, field := range []string{"prompt_cache_key", "user"} {
		var v string
		if json.Unmarshal(top[field], &v) == nil && v != "" {
			return protocol.AffinityHint{Source: protocol.AffinityUser, Value: []byte(v)}
		}
	}
	m0, _ := jsonx.ArrayElem(top["messages"], 0)
	m1, _ := jsonx.ArrayElem(top["messages"], 1)
	parts := [][]byte{nonNull(top["tools"]), nonNull(m0), nonNull(m1)}
	if len(parts[0])+len(parts[1])+len(parts[2]) == 0 {
		return protocol.AffinityHint{}
	}
	return protocol.AffinityHint{Source: protocol.AffinityPrefix, Value: bytes.Join(parts, []byte{0})}
}

func nonNull(raw json.RawMessage) []byte {
	if jsonx.IsNull(raw) {
		return nil
	}
	return bytes.TrimSpace(raw)
}

func invalid(msg string) *apperr.Error { return apperr.New(apperr.CodeInvalidRequest, msg) }

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

// ErrorType 返回错误码对应的 OpenAI error.type（未列出的一律为 server_error）。
func ErrorType(c apperr.Code) string {
	if t, ok := errorType[c]; ok {
		return t
	}
	return "server_error"
}

// WriteError 写 {"error":{"message","type","param","code"}}（spec/08 §2.5）。
func (Ingress) WriteError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{"error": map[string]any{
		"message": err.Message, "type": ErrorType(err.Code), "param": nil, "code": string(err.Code),
	}})
}

// StreamError 实现 spec/08 §2.6 的 OpenAI 错误事件（之后不发送 [DONE]）。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	data, _ := json.Marshal(map[string]any{ // 只含字符串的 map，序列化不会失败
		"error": map[string]string{"message": err.Message, "type": ErrorType(err.Code), "code": string(err.Code)},
	})
	return sse.Event{Data: data}
}

type catalogModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// WriteCatalog 实现 spec/08 §2.8 的 OpenAI 格式模型列表。
func (Ingress) WriteCatalog(w http.ResponseWriter, models []protocol.ModelInfo) {
	data := make([]catalogModel, 0, len(models))
	for _, m := range models {
		data = append(data, catalogModel{ID: m.ID, Object: "model", OwnedBy: "universal-ai-gateway"})
	}
	b, err := json.Marshal(map[string]any{"object": "list", "data": data})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b) // 写客户端失败无法补救
}
