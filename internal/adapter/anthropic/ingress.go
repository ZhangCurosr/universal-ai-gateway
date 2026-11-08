package anthropic

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

// Ingress 是 Anthropic 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var _ protocol.Ingress = Ingress{}

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// OpMessages 是 Messages 调用的操作名。
const OpMessages = "messages"

// Routes 声明 POST /v1/messages 与 GET /v1/models（IN-A01）。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/messages", Kind: protocol.RouteCall, Op: OpMessages},
		{Method: http.MethodGet, Path: "/v1/models", Kind: protocol.RouteCatalog},
	}
}

// Claims 认领带 anthropic-version 头的请求（IN-A02，用于 GET /v1/models 与 OpenAI 冲突时）。
func (Ingress) Claims(r *http.Request) bool {
	return r.Header.Get("anthropic-version") != ""
}

// Parse 实现 IN-G01 ~ IN-G06、IN-L01 ~ IN-L04 与 AFH-01 ~ AFH-04（spec/08 §2）。
// 只解析一次顶层对象，只读取少数字段（IN-G06）。
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
	if raw := top["max_tokens"]; !jsonx.IsNull(raw) { // IN-L04
		var n int64
		if json.Unmarshal(raw, &n) != nil || n <= 0 {
			return nil, invalid("max_tokens: must be a positive integer")
		}
		est[pricing.UnitOutputTokens] = n
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

// affinityHint 实现 AFH-01 ~ AFH-04：metadata.user_id 优先；否则 tools + system + messages[0] 的原始字节以 0x00 拼接。
func affinityHint(top map[string]json.RawMessage) protocol.AffinityHint {
	if uid, ok := jsonx.GetString(top["metadata"], "user_id"); ok && uid != "" {
		return protocol.AffinityHint{Source: protocol.AffinityUser, Value: []byte(uid)}
	}
	first, _ := jsonx.ArrayElem(top["messages"], 0)
	parts := [][]byte{nonNull(top["tools"]), nonNull(top["system"]), nonNull(first)}
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
	apperr.CodeInvalidRequest:      "invalid_request_error",
	apperr.CodeUnauthenticated:     "authentication_error",
	apperr.CodeInsufficientQuota:   "billing_error",
	apperr.CodePermissionDenied:    "permission_error",
	apperr.CodeNotFound:            "not_found_error",
	apperr.CodeConflict:            "invalid_request_error",
	apperr.CodeRequestTooLarge:     "request_too_large",
	apperr.CodeRateLimited:         "rate_limit_error",
	apperr.CodeUpstreamUnavailable: "overloaded_error",
	apperr.CodeOverloaded:          "overloaded_error",
	apperr.CodeTimeout:             "timeout_error",
}

// ErrorType 返回错误码对应的 Anthropic error.type（未列出的一律为 api_error）。
func ErrorType(c apperr.Code) string {
	if t, ok := errorType[c]; ok {
		return t
	}
	return "api_error"
}

// WriteError 写 {"type":"error","error":{"type","message"},"request_id"}（spec/08 §2.5）。
func (Ingress) WriteError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{
		"type":       "error",
		"error":      map[string]any{"type": ErrorType(err.Code), "message": err.Message},
		"request_id": requestID,
	})
}

// StreamError 实现 spec/08 §2.6：event: error，data: {"type":"error","error":{"type","message"}}。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	data, _ := json.Marshal(map[string]any{ // 只含字符串的 map，序列化不会失败
		"type":  "error",
		"error": map[string]string{"type": ErrorType(err.Code), "message": err.Message},
	})
	return sse.Event{Name: "error", Data: data}
}

type catalogModel struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// WriteCatalog 实现 spec/08 §2.8 的 Anthropic 格式模型列表。
func (Ingress) WriteCatalog(w http.ResponseWriter, models []protocol.ModelInfo) {
	data := make([]catalogModel, 0, len(models))
	for _, m := range models {
		data = append(data, catalogModel{Type: "model", ID: m.ID, DisplayName: m.ID, CreatedAt: "1970-01-01T00:00:00Z"})
	}
	var first, last *string
	if len(models) > 0 {
		first, last = &models[0].ID, &models[len(models)-1].ID
	}
	b, err := json.Marshal(map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b) // 写客户端失败无法补救
}
