package gemini

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

// Ingress 是 Gemini API 入站适配器（客户端 → 网关）。零值可用。
type Ingress struct{}

var _ protocol.Ingress = Ingress{}

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// OpModelsCall 是 Gemini models API 调用的操作名。
const OpModelsCall = "models.call"

// Routes 声明 POST /v1beta/models/{target} 与 GET /v1beta/models（IN-G-01）。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1beta/models/{target}", Kind: protocol.RouteCall, Op: OpModelsCall, Body: protocol.BodyBuffered},
		{Method: http.MethodGet, Path: "/v1beta/models", Kind: protocol.RouteCatalog},
	}
}

// Claims 恒为 false（IN-G-10：路由不与其他协议冲突）。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 IN-G-02 ~ IN-G-08（spec/08 §10.2）。
func (Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	target := r.PathValue("target")
	if target == "" {
		p := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
		target = strings.TrimPrefix(p, "/")
	}
	idx := strings.LastIndex(target, ":")
	if idx == -1 {
		return nil, apperr.New(apperr.CodeInvalidRequest, "path must be /v1beta/models/{model}:{method}")
	}
	modelPart := target[:idx]
	method := target[idx+1:]
	model := strings.TrimPrefix(modelPart, "models/")
	if model == "" || method == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "path must be /v1beta/models/{model}:{method}")
	}
	switch method {
	case "generateContent", "streamGenerateContent", "countTokens":
	default:
		return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("method %q is not supported", method))
	}
	if method == "streamGenerateContent" {
		if r.URL.Query().Get("alt") != "sse" {
			return nil, apperr.New(apperr.CodeInvalidRequest, "streamGenerateContent requires alt=sse")
		}
	}

	top, err := jsonx.Object(body)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidRequest, "request body must be a JSON object")
	}

	var est pricing.Usage
	var affinity protocol.AffinityHint
	if method == "countTokens" {
		est = pricing.Usage{
			pricing.UnitInputTokens:     0,
			pricing.UnitOutputTokens:    0,
			pricing.UnitCacheReadTokens: 0,
		}
		// countTokens 不设亲和（IN-G-08）
	} else {
		est = pricing.Usage{
			pricing.UnitInputTokens: pricing.EstimateInputTokens(len(body)),
		}
		if gcRaw, ok := top["generationConfig"]; ok && !jsonx.IsNull(gcRaw) {
			gcMap, err := jsonx.Object(gcRaw)
			if err != nil {
				return nil, apperr.New(apperr.CodeInvalidRequest, "generationConfig must be an object")
			}
			if motRaw, ok := gcMap["maxOutputTokens"]; ok && !jsonx.IsNull(motRaw) {
				var n int64
				if err := json.Unmarshal(motRaw, &n); err != nil || n <= 0 {
					return nil, apperr.New(apperr.CodeInvalidRequest, "maxOutputTokens must be a positive integer")
				}
				est[pricing.UnitOutputTokens] = n
			}
		}
		affinity = affinityHint(top)
	}

	return &protocol.Envelope{
		Protocol: Protocol,
		Op:       rt.Op,
		Model:    model,
		Stream:   method == "streamGenerateContent",
		Estimate: est,
		Affinity: affinity,
		Body:     body,
		Header:   r.Header,
		Params:   map[string]string{"method": method},
	}, nil
}

// affinityHint 实现 IN-G-08：systemInstruction + tools + contents[0]。
func affinityHint(top map[string]json.RawMessage) protocol.AffinityHint {
	c0, _ := jsonx.ArrayElem(top["contents"], 0)
	parts := [][]byte{nonNull(top["systemInstruction"]), nonNull(top["tools"]), nonNull(c0)}
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

var errorStatus = map[apperr.Code]string{
	apperr.CodeInvalidRequest:      "INVALID_ARGUMENT",
	apperr.CodeRequestTooLarge:     "INVALID_ARGUMENT",
	apperr.CodeUnauthenticated:     "UNAUTHENTICATED",
	apperr.CodePermissionDenied:    "PERMISSION_DENIED",
	apperr.CodeNotFound:            "NOT_FOUND",
	apperr.CodeConflict:            "ABORTED",
	apperr.CodeRateLimited:         "RESOURCE_EXHAUSTED",
	apperr.CodeInsufficientQuota:   "RESOURCE_EXHAUSTED",
	apperr.CodeNotImplemented:      "UNIMPLEMENTED",
	apperr.CodeTimeout:             "DEADLINE_EXCEEDED",
	apperr.CodeOverloaded:          "UNAVAILABLE",
	apperr.CodeUpstreamUnavailable: "UNAVAILABLE",
	apperr.CodeInternal:            "INTERNAL",
	apperr.CodeUpstreamError:       "INTERNAL",
}

// ErrorStatus 返回错误码对应的 Google API status 字段。
func ErrorStatus(c apperr.Code) string {
	if s, ok := errorStatus[c]; ok {
		return s
	}
	return "INTERNAL"
}

// WriteError 写 {"error":{"code","message","status"}}（spec/08 §10.2）。
func (Ingress) WriteError(w http.ResponseWriter, requestID string, err *apperr.Error) {
	status := ErrorStatus(err.Code)
	apperr.WriteJSON(w, err, map[string]any{"error": map[string]any{
		"code":    err.Code.HTTPStatus(),
		"message": err.Message,
		"status":  status,
	}})
}

// StreamError 实现 spec/08 §10.2 的 Gemini SSE 错误事件。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	status := ErrorStatus(err.Code)
	data, _ := json.Marshal(map[string]any{"error": map[string]any{ // 只含字符串与整数的 map，序列化不会失败
		"code":    err.Code.HTTPStatus(),
		"message": err.Message,
		"status":  status,
	}})
	return sse.Event{Data: data}
}

type catalogModel struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

// WriteCatalog 实现 spec/08 §10.2 的模型列表。
func (Ingress) WriteCatalog(w http.ResponseWriter, models []protocol.ModelInfo) {
	data := make([]catalogModel, 0, len(models))
	methods := []string{"generateContent", "streamGenerateContent", "countTokens"}
	for _, m := range models {
		id := strings.TrimPrefix(m.ID, "models/")
		data = append(data, catalogModel{
			Name:                       "models/" + id,
			DisplayName:                id,
			SupportedGenerationMethods: methods,
		})
	}
	b, err := json.Marshal(map[string]any{"models": data})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b) // 写客户端失败无法补救
}
