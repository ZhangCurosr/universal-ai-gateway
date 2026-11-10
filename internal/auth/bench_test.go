package auth

import (
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// BenchmarkLookup 测试数据面热路径上的鉴权三部曲：
// ExtractCredential + HashToken + Index.Lookup（spec/02 §10.1 BEN-01，spec/05 §3）。
func BenchmarkLookup(b *testing.B) {
	b.ReportAllocs()

	pepper := []byte("benchmark-pepper-secret-32bytes!")
	plain, _, err := GenerateToken()
	if err != nil {
		b.Fatalf("GenerateToken failed: %v", err)
	}
	tokenHash := HashToken(pepper, plain)

	ix := NewIndex()
	ix.Replace([]store.Client{
		{
			ID:        1,
			Name:      "bench-client",
			Role:      store.RoleUser,
			TokenHash: tokenHash,
		},
	})

	header := http.Header{}
	header.Set("Authorization", "Bearer "+plain)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cred, ok := ExtractCredential(header)
			if !ok {
				b.Fatal("ExtractCredential failed")
			}
			h := HashToken(pepper, cred)
			p, found := ix.Lookup(h)
			if !found || p == nil {
				b.Fatal("Lookup failed")
			}
		}
	})
}
