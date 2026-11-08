package obs

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"strings"
)

type requestIDKey struct{}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewRequestID 生成 "req_" + 24 位小写 base32 随机串（MW-01、LIFE-02）。
func NewRequestID() string {
	b := make([]byte, 15) // 15 字节 = 24 个 base32 字符
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在受支持的平台上不会失败；失败说明系统熵源损坏，属于不可恢复的编程环境错误
		panic("crypto/rand failed: " + err.Error())
	}
	return "req_" + strings.ToLower(b32.EncodeToString(b))
}

// WithRequestID 把请求 ID 存入 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom 取出请求 ID；不存在返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
