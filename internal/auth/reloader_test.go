package auth

import (
	"context"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

type fakeLister struct{ clients []store.Client }

func (f fakeLister) ListClients(context.Context) ([]store.Client, error) { return f.clients, nil }

// spec/05 §3.4
func TestReloaderReplacesIndex(t *testing.T) {
	t.Skip("TODO(T03)")
	ix := NewIndex()
	r := &Reloader{Store: fakeLister{clients: []store.Client{{ID: 7, TokenHash: "h7"}}}, Index: ix}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if p, ok := ix.Lookup("h7"); !ok || p.ClientID != 7 {
		t.Fatalf("after Reload Lookup(h7) = %+v, %v", p, ok)
	}
}
