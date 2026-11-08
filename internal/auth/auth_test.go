package auth

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func TestGenerateToken(t *testing.T) {
	plain, display, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) || len(plain) != 52 {
		t.Fatalf("token = %q (len %d), want uai_live_ + 43 chars", plain, len(plain))
	}
	if display != plain[:15]+"…" {
		t.Fatalf("display = %q", display)
	}
	other, _, _ := GenerateToken()
	if other == plain {
		t.Fatal("two tokens are identical")
	}
}

func TestHashTokenDeterministic(t *testing.T) {
	p := []byte("pepper")
	if HashToken(p, "a") != HashToken(p, "a") || HashToken(p, "a") == HashToken([]byte("other"), "a") {
		t.Fatal("HashToken not deterministic or ignores pepper")
	}
}

func TestExtractCredential(t *testing.T) {
	tok := TokenPrefix + "abc"
	tests := []struct {
		name   string
		header map[string]string
		want   string
		ok     bool
	}{
		{"AUTH-01 bearer", map[string]string{"Authorization": "Bearer " + tok}, tok, true},
		{"AUTH-01 bearer case-insensitive", map[string]string{"Authorization": "bearer  " + tok + " "}, tok, true},
		{"AUTH-01 x-api-key", map[string]string{"x-api-key": tok}, tok, true},
		{"AUTH-01 x-goog-api-key", map[string]string{"x-goog-api-key": tok}, tok, true},
		{"AUTH-01 bearer wins", map[string]string{"Authorization": "Bearer " + tok, "x-api-key": TokenPrefix + "other"}, tok, true},
		{"AUTH-02 upstream key rejected", map[string]string{"x-api-key": "sk-ant-api03-xyz"}, "", false},
		{"AUTH-04 too long", map[string]string{"x-api-key": TokenPrefix + strings.Repeat("a", 300)}, "", false},
		{"none", map[string]string{}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.header {
				h.Set(k, v)
			}
			got, ok := ExtractCredential(h)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("ExtractCredential() = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestIndexReplaceAndLookup(t *testing.T) {
	ix := NewIndex()
	if _, ok := ix.Lookup("h"); ok {
		t.Fatal("empty index lookup ok = true")
	}
	ix.Replace([]store.Client{
		{ID: 1, TokenHash: "h1", Role: store.RoleUser, AllowedModels: []string{"m1"}},
		{ID: 2, TokenHash: "h2", Disabled: true}, // AUTH-07
	})
	p, ok := ix.Lookup("h1")
	if !ok || p.ClientID != 1 || !p.ModelAllowed("m1") || p.ModelAllowed("m2") {
		t.Fatalf("Lookup(h1) = %+v, %v", p, ok)
	}
	if p, ok := ix.Lookup("h2"); !ok || !p.Disabled {
		t.Fatalf("disabled client missing from index: %+v, %v", p, ok)
	}
	if !(&Principal{}).ModelAllowed("anything") { // AUTH-09
		t.Fatal("empty allow-list should allow all models")
	}
}

func TestIndexConcurrentReplace(t *testing.T) {
	ix := NewIndex()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ix.Replace([]store.Client{{ID: 1, TokenHash: "h"}})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ix.Lookup("h")
			}
		}()
	}
	wg.Wait()
}
