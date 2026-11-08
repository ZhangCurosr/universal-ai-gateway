package core

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// spec/07 §7
func TestAffinityKeySources(t *testing.T) {
	t.Skip("TODO(T10)")
	withHeader := http.Header{}
	withHeader.Set(SessionHeader, "my-session")
	user := protocol.AffinityHint{Source: protocol.AffinityUser, Value: []byte("user_abc")}
	prefix := protocol.AffinityHint{Source: protocol.AffinityPrefix, Value: []byte("tools+system+first-message")}

	tests := []struct {
		name   string
		h      http.Header
		hint   protocol.AffinityHint
		prefix string
	}{
		{"header wins over hint", withHeader, user, "c7:h:"},
		{"user hint", http.Header{}, user, "c7:u:"},
		{"prefix hint", http.Header{}, prefix, "c7:p:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AffinityKey(7, tt.h, tt.hint)
			if !strings.HasPrefix(got, tt.prefix) || len(got) != len(tt.prefix)+16 {
				t.Fatalf("AffinityKey() = %q, want prefix %q + 16 hex chars", got, tt.prefix)
			}
		})
	}
	if got := AffinityKey(7, http.Header{}, protocol.AffinityHint{}); got != "" {
		t.Fatalf("AffinityKey(no hint) = %q, want empty", got)
	}
	if AffinityKey(1, withHeader, user) == AffinityKey(2, withHeader, user) {
		t.Fatal("affinity key not scoped by client")
	}
	strict := protocol.AffinityHint{Source: protocol.AffinityPin, Value: []byte("job-1"), Strict: true}
	if got := AffinityKey(7, withHeader, strict); got != AffinityKey(7, nil, protocol.AffinityHint{Source: protocol.AffinityPin, Value: []byte("job-1")}) ||
		!strings.HasPrefix(got, "c7:j:") {
		t.Fatalf("AffinityKey(strict) = %q, want pin key that ignores the session header", got)
	}
	long := http.Header{}
	long.Set(SessionHeader, strings.Repeat("x", 200)) // > 128 字节的头被忽略，退回线索
	if got := AffinityKey(7, long, user); !strings.HasPrefix(got, "c7:u:") {
		t.Fatalf("AffinityKey(oversized header) = %q, want user hint", got)
	}
}
