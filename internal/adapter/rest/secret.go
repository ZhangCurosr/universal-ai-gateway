package rest

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// parsedSecret 表示解析后的上游凭据放置指令。
type parsedSecret struct {
	headerName  string
	headerValue string
	queryParam  string
	queryValue  string
}

// invalidKeyError 返回格式非法错误（RS-20）。
// 严格遵守规范：错误消息固定为「rest upstream key format is invalid」，不含任何凭据明文片段。
func invalidKeyError() error {
	return &protocol.AttemptError{
		Outcome: protocol.Outcome{
			Kind:   protocol.OutcomeAuthFailed,
			Status: http.StatusUnauthorized,
		},
		Err: errors.New("rest upstream key format is invalid"),
	}
}

// parseSecret 解析上游 Key 明文（§8.3.3、RS-20）。
func parseSecret(raw []byte) (*parsedSecret, error) {
	secret := string(raw)

	switch {
	case strings.HasPrefix(secret, "bearer:"):
		token := secret[len("bearer:"):]
		if strings.ContainsAny(token, "\r\n") {
			return nil, invalidKeyError()
		}
		return &parsedSecret{
			headerName:  "Authorization",
			headerValue: "Bearer " + token,
		}, nil

	case strings.HasPrefix(secret, "header:"):
		rest := secret[len("header:"):]
		idx := strings.Index(rest, ":")
		if idx <= 0 { // 缺少冒号或头名为空
			return nil, invalidKeyError()
		}
		name := rest[:idx]
		val := rest[idx+1:]
		if !isValidHeaderToken(name) || isForbiddenKeyHeader(name) {
			return nil, invalidKeyError()
		}
		if strings.ContainsAny(val, "\r\n") {
			return nil, invalidKeyError()
		}
		return &parsedSecret{
			headerName:  name,
			headerValue: val,
		}, nil

	case strings.HasPrefix(secret, "query:"):
		rest := secret[len("query:"):]
		idx := strings.Index(rest, ":")
		if idx <= 0 { // 缺少冒号或参数名为空
			return nil, invalidKeyError()
		}
		name := rest[:idx]
		val := rest[idx+1:]
		if !isValidQueryParamName(name) {
			return nil, invalidKeyError()
		}
		return &parsedSecret{
			queryParam: name,
			queryValue: val,
		}, nil

	case strings.HasPrefix(secret, "basic:"):
		rest := secret[len("basic:"):]
		idx := strings.Index(rest, ":")
		if idx < 0 { // 缺少冒号（必须有 user:password 拆分）
			return nil, invalidKeyError()
		}
		user := rest[:idx]
		pass := rest[idx+1:]
		if strings.ContainsAny(user, ":\r\n") || strings.ContainsAny(pass, "\r\n") {
			return nil, invalidKeyError()
		}
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		return &parsedSecret{
			headerName:  "Authorization",
			headerValue: "Basic " + auth,
		}, nil

	default:
		// 兜底为 bearer:<整个明文>（§8.3.3）
		if strings.ContainsAny(secret, "\r\n") {
			return nil, invalidKeyError()
		}
		return &parsedSecret{
			headerName:  "Authorization",
			headerValue: "Bearer " + secret,
		}, nil
	}
}

// isValidHeaderToken 校验 RFC 9110 token 字符集。
func isValidHeaderToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

// isValidQueryParamName 校验查询参数名（RS-20，只允许 [A-Za-z0-9_.-]）。
func isValidQueryParamName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// isForbiddenKeyHeader 检查 header: 方式下是否使用了禁止的头名。
// 规则：除 Authorization 与 X-Api-Key 之外的 RS-12 丢弃列表成员均被禁止。
func isForbiddenKeyHeader(name string) bool {
	lower := strings.ToLower(name)
	if lower == "authorization" || lower == "x-api-key" {
		return false
	}
	return isDropForwardHeader(name)
}

// isDropForwardHeader 检查是否属于 RS-12 的丢弃列表。
func isDropForwardHeader(name string) bool {
	if !isValidHeaderToken(name) {
		return true
	}
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "proxy-authorization", "cookie", "host",
		"content-length", "transfer-encoding", "connection", "upgrade",
		"te", "trailer", "keep-alive", "x-api-key", "x-goog-api-key":
		return true
	}
	if strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "x-uaig-") {
		return true
	}
	return false
}
