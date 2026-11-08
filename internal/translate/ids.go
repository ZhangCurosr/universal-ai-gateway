package translate

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// lowerBase32 是不带填充的小写 base32 编码（RFC 4648 字母表）。
var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newGatewayID 生成网关签发的 ID：prefix + 24 位小写 base32（RX-05）。
// 15 字节随机数恰好编码为 24 个字符；必须用 crypto/rand（AGENTS §3 #3）。
func newGatewayID(prefix string) string {
	var b [15]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read 按文档从不返回错误（Go 1.24+ 失败时直接终止进程）
	var sb strings.Builder
	sb.WriteString(prefix)
	sb.WriteString(lowerBase32.EncodeToString(b[:]))
	return sb.String()
}
