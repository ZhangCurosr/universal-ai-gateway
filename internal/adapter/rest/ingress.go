package rest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Ingress 是通用 REST 入站适配器（客户端 → 网关）。零值可用（没有接口声明）。
type Ingress struct {
	tools *toolTable // 配置声明的接口（RS-40）；构造后只读，nil = 没有工具
	apis  *apiTable  // 声明式 API 能力（DA-09）；构造后只读，nil = 没有声明
}

var (
	_ protocol.Ingress    = Ingress{}
	_ protocol.ToolSource = Ingress{}
)

// Protocol 返回 Protocol。
func (Ingress) Protocol() protocol.Protocol { return Protocol }

// Routes 声明通用 REST 的 7 条路由（RS-02）。无目录路由。
// 注意：GET 路由通过 http.ServeMux 自然匹配 HEAD 请求。
func (Ingress) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodGet, Path: "/rest/{service}/{path...}", Kind: protocol.RouteCall, Op: OpGet, Body: protocol.BodyNone},
		{Method: http.MethodDelete, Path: "/rest/{service}/{path...}", Kind: protocol.RouteCall, Op: OpDelete, Body: protocol.BodyNone},
		{Method: http.MethodPut, Path: "/rest/{service}/{path...}", Kind: protocol.RouteCall, Op: OpPut, Body: protocol.BodyBuffered},
		{Method: http.MethodPost, Path: "/rest/{service}/{path...}", Kind: protocol.RouteCall, Op: OpPost, Body: protocol.BodyStream},
		{Method: http.MethodPatch, Path: "/rest/{service}/{path...}", Kind: protocol.RouteCall, Op: OpPatch, Body: protocol.BodyStream},
		{Method: http.MethodPost, Path: "/rest-idem/{service}/{path...}", Kind: protocol.RouteCall, Op: OpPostIdem, Body: protocol.BodyBuffered},
		{Method: http.MethodPatch, Path: "/rest-idem/{service}/{path...}", Kind: protocol.RouteCall, Op: OpPatchIdem, Body: protocol.BodyBuffered},
	}
}

// Claims 永远返回 false（RS-01，不是 Fallback，无冲突端点）。
func (Ingress) Claims(*http.Request) bool { return false }

// Parse 实现 RS-02 ~ RS-06。
func (in Ingress) Parse(r *http.Request, rt protocol.Route, body []byte) (*protocol.Envelope, error) {
	service := strings.TrimSpace(r.PathValue("service"))
	if service == "" {
		return nil, apperr.New(apperr.CodeInvalidRequest, "service is required")
	}

	// 提取 {path...} 的转义形式（RS-04）
	subpath := extractSubpath(r.URL.EscapedPath())

	// 路径安全检查（RS-05）
	if err := validatePathSafety(subpath); err != nil {
		return nil, err
	}

	env := &protocol.Envelope{
		Protocol: Protocol,
		Op:       rt.Op,
		Model:    service,
		Stream:   false,
		Estimate: pricing.Usage{UnitRequests: 1},
		Params: map[string]string{
			"method": r.Method,
			"path":   subpath,
			"query":  r.URL.RawQuery,
		},
		Body:   body,
		Header: r.Header,
	}
	if ep, vars := in.apis.lookup(service).match(r.Method, subpath); ep != nil {
		if ep.Usage != nil {
			env.Estimate = pricing.Usage{} // 声明了用量：预留由 reserve 决定（DA-07）
		}
		if ep.Flow != nil && ep.Flow.Upload != nil && rt.Body == protocol.BodyStream { // FL-03：签名地址只接受定长上传
			if r.ContentLength <= 0 {
				return nil, apperr.New(apperr.CodeInvalidRequest, "Content-Length is required (chunked uploads are not supported)")
			}
			env.Params["size"] = strconv.FormatInt(r.ContentLength, 10)
		}
		if ep.PinnedBy != "" { // DA-09：只用创建任务的 Key，不换 Key
			env.Affinity = protocol.AffinityHint{Source: protocol.AffinityPin, Value: []byte(pinValue(service, vars[ep.PinnedBy])), Strict: true}
		}
	}
	return env, nil
}

// extractSubpath 从 EscapedPath 中提取 /rest/{service} 或 /rest-idem/{service} 之后的部分（RS-04）。
func extractSubpath(escaped string) string {
	var rem string
	if strings.HasPrefix(escaped, "/rest/") {
		rem = escaped[len("/rest/"):]
	} else if strings.HasPrefix(escaped, "/rest-idem/") {
		rem = escaped[len("/rest-idem/"):]
	} else {
		return strings.TrimPrefix(escaped, "/")
	}
	idx := strings.Index(rem, "/")
	if idx >= 0 {
		return rem[idx:]
	}
	return ""
}

// validatePathSafety 检查路径防穿越（RS-05）。
func validatePathSafety(subpath string) error {
	if subpath == "" {
		return nil
	}
	segments := strings.Split(subpath, "/")
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if strings.ContainsAny(seg, "\\\x00") {
			return apperr.New(apperr.CodeInvalidRequest, "path must not contain dot segments")
		}
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return apperr.New(apperr.CodeInvalidRequest, "path must not contain dot segments")
		}
		if dec == "." || dec == ".." {
			return apperr.New(apperr.CodeInvalidRequest, "path must not contain dot segments")
		}
		if strings.ContainsAny(dec, "\\\x00") {
			return apperr.New(apperr.CodeInvalidRequest, "path must not contain dot segments")
		}
	}
	return nil
}

// WriteError 输出 JSON 错误响应（RS-01）。
func (Ingress) WriteError(w http.ResponseWriter, _ string, err *apperr.Error) {
	apperr.WriteJSON(w, err, map[string]any{
		"error": map[string]any{
			"code":    string(err.Code),
			"message": err.Message,
		},
	})
}

// StreamError 返回流式错误事件。
func (Ingress) StreamError(err *apperr.Error) sse.Event {
	data, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    string(err.Code),
			"message": err.Message,
		},
	})
	return sse.Event{Data: data}
}

// WriteCatalog 实现没有目录路由的 404 响应。
func (Ingress) WriteCatalog(w http.ResponseWriter, _ []protocol.ModelInfo) {
	apperr.WriteJSON(w, apperr.New(apperr.CodeNotFound, "catalog is not supported for rest protocol"), map[string]any{
		"error": map[string]any{
			"code":    string(apperr.CodeNotFound),
			"message": "catalog is not supported for rest protocol",
		},
	})
}
