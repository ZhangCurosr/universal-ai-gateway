package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
)

// spec/07 §7
func TestAffinityKeySources(t *testing.T) {
	t.Skip("TODO(T10)")
	withHeader := http.Header{}
	withHeader.Set(SessionHeader, "my-session")
	anthropicBody := []byte(`{"model":"m","metadata":{"user_id":"user_abc_session_123"},"system":"s","messages":[{"role":"user","content":"hi"}]}`)
	noMeta := []byte(`{"model":"m","system":"s","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x"}]}`)
	openaiBody := []byte(`{"model":"m","user":"u1","messages":[{"role":"user","content":"hi"}]}`)

	tests := []struct {
		name   string
		proto  provider.Protocol
		h      http.Header
		body   []byte
		prefix string
	}{
		{"header wins", provider.ProtocolAnthropic, withHeader, anthropicBody, "c7:h:"},
		{"anthropic metadata.user_id", provider.ProtocolAnthropic, http.Header{}, anthropicBody, "c7:u:"},
		{"anthropic prefix hash", provider.ProtocolAnthropic, http.Header{}, noMeta, "c7:p:"},
		{"openai user", provider.ProtocolOpenAIChat, http.Header{}, openaiBody, "c7:u:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AffinityKey(tt.proto, 7, tt.h, tt.body)
			if !strings.HasPrefix(got, tt.prefix) || len(got) != len(tt.prefix)+16 {
				t.Fatalf("AffinityKey() = %q, want prefix %q + 16 hex chars", got, tt.prefix)
			}
		})
	}

	// 前缀哈希只依赖第一条消息：追加后续轮次不改变亲和键
	longer := []byte(`{"model":"m","system":"s","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x"},{"role":"user","content":"more"}]}`)
	if AffinityKey(provider.ProtocolAnthropic, 7, http.Header{}, noMeta) != AffinityKey(provider.ProtocolAnthropic, 7, http.Header{}, longer) {
		t.Fatal("prefix affinity key changed when later turns were appended")
	}
	// 不同客户端不共享亲和键
	if AffinityKey(provider.ProtocolAnthropic, 1, withHeader, nil) == AffinityKey(provider.ProtocolAnthropic, 2, withHeader, nil) {
		t.Fatal("affinity key not scoped by client")
	}
}
