// Package auth 实现客户端 Token 生成、哈希、凭据提取与内存索引（spec/05 §3）。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// TokenPrefix 是所有客户端 Token 的前缀。
const TokenPrefix = "uai_live_"

const (
	tokenRandomBytes = 32
	displayChars     = 15
	maxCredentialLen = 256 // AUTH-04
)

// GenerateToken 生成新的客户端 Token 及其展示串（spec/05 §3.1）。
func GenerateToken() (plain, display string, err error) {
	b := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return plain, DisplayOf(plain), nil
}

// DisplayOf 返回 Token 的展示串：前 15 个字符 + "…"。
func DisplayOf(plain string) string {
	if len(plain) <= displayChars {
		return plain
	}
	return plain[:displayChars] + "…"
}

// HashToken 返回 hex(HMAC-SHA256(pepper, plain))，用于索引与存储。也用于上游 Key 指纹。
func HashToken(pepper []byte, plain string) string {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(plain))
	return hex.EncodeToString(m.Sum(nil))
}

// ExtractCredential 按 AUTH-01 ~ AUTH-04 从请求头中取出客户端 Token。
func ExtractCredential(h http.Header) (string, bool) {
	candidates := []string{bearer(h.Get("Authorization")), h.Get("x-api-key"), h.Get("x-goog-api-key")}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		// AUTH-01：返回第一个非空值；AUTH-02 / AUTH-04 对该值做校验
		if len(c) > maxCredentialLen || !strings.HasPrefix(c, TokenPrefix) {
			return "", false
		}
		return c, true
	}
	return "", false
}

func bearer(v string) string {
	const p = "bearer "
	if len(v) >= len(p) && strings.EqualFold(v[:len(p)], p) {
		return v[len(p):]
	}
	return ""
}
