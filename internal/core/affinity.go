package core

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// SessionHeader 是客户端显式指定会话的请求头（spec/07 §7 优先级 1）。适用于所有协议。
const SessionHeader = "X-UAIG-Session"

// AffinityKey 按 spec/07 §7 计算亲和键：请求头优先，其次是入站适配器给出的线索；都没有返回 ""。
// hint.Strict 为 true 时忽略请求头，只用线索（任务固定不能被客户端头覆盖）。
// 格式："c<clientID>:<h|u|p|j>:<hex(sha256(value))[:16]>"。
// 记录 Response.Pins 时，内核对每个 pin 调用 AffinityKey(clientID, nil, AffinityHint{Source: AffinityPin, Value: pin})。
func AffinityKey(clientID int64, h http.Header, hint protocol.AffinityHint) string {
	source, value := "", []byte(nil)
	if !hint.Strict && h != nil {
		if v := h.Get(SessionHeader); v != "" && len(v) <= maxSessionHeaderBytes {
			source, value = "h", []byte(v)
		}
	}
	if source == "" && len(hint.Value) > 0 && hint.Source != "" {
		source, value = hint.Source, hint.Value
	}
	if source == "" {
		return ""
	}
	sum := sha256.Sum256(value)
	return "c" + strconv.FormatInt(clientID, 10) + ":" + source + ":" + hex.EncodeToString(sum[:])[:16]
}

// maxSessionHeaderBytes 是 X-UAIG-Session 的长度上限；超长视为不存在（spec/07 §7）。
const maxSessionHeaderBytes = 128
