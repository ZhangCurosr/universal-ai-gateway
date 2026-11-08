package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

type fakeLister struct {
	clients []store.Client
	err     error
}

func (f fakeLister) ListClients(context.Context) ([]store.Client, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.clients, nil
}

// spec/05 §3.4
func TestReloaderReplacesIndex(t *testing.T) {
	ix := NewIndex()
	r := &Reloader{Store: fakeLister{clients: []store.Client{{ID: 7, TokenHash: "h7"}}}, Index: ix}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if p, ok := ix.Lookup("h7"); !ok || p.ClientID != 7 {
		t.Fatalf("after Reload Lookup(h7) = %+v, %v", p, ok)
	}
}

func TestReloaderErrorKeepsOldIndex(t *testing.T) {
	ix := NewIndex()
	ix.Replace([]store.Client{{ID: 42, TokenHash: "h42"}})

	underlying := errors.New("database disk image is malformed")
	r := &Reloader{
		Store: fakeLister{err: underlying},
		Index: ix,
	}

	err := r.Reload(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, underlying) {
		t.Fatalf("expected error wrapping underlying error, got %v", err)
	}

	// Verify the old index snapshot was not replaced
	if ix.Len() != 1 {
		t.Fatalf("expected index len 1, got %d", ix.Len())
	}
	if p, ok := ix.Lookup("h42"); !ok || p.ClientID != 42 {
		t.Fatalf("expected client 42 to remain in index, got %+v, %v", p, ok)
	}
}
