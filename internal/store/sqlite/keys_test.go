package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func sampleKey(fp string) *store.UpstreamKey {
	return &store.UpstreamKey{Pool: "p1", Name: "k", Fingerprint: fp, SecretHint: "sk-a****wxyz", Weight: 1, MaxInFlight: 4, State: store.KeyStateReady}
}

// STO-06：seal 必须拿到真实自增 id，密文写入该行
func TestInsertUpstreamKeySealUsesID(t *testing.T) {
	t.Skip("TODO(T02)")
	ctx := context.Background()
	db := newTestDB(t)
	var sealedID int64
	id, err := db.InsertUpstreamKey(ctx, sampleKey("fp1"), func(id int64) (crypto.Sealed, error) {
		sealedID = id
		return crypto.Sealed{KEKVersion: 1, WrappedDEK: []byte("w"), Ciphertext: []byte("c")}, nil
	})
	if err != nil {
		t.Fatalf("InsertUpstreamKey() error = %v", err)
	}
	if sealedID != id {
		t.Fatalf("seal called with id %d, row id %d", sealedID, id)
	}
	got, err := db.GetUpstreamKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Sealed.Ciphertext) != "c" || string(got.Sealed.WrappedDEK) != "w" || got.Sealed.KEKVersion != 1 {
		t.Fatalf("stored sealed = %+v", got.Sealed)
	}
	if _, err := db.InsertUpstreamKey(ctx, sampleKey("fp1"), func(int64) (crypto.Sealed, error) {
		return crypto.Sealed{KEKVersion: 1}, nil
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate fingerprint error = %v, want ErrConflict", err)
	}
}

// STO-06：seal 出错时整行回滚
func TestInsertUpstreamKeySealErrorRollsBack(t *testing.T) {
	t.Skip("TODO(T02)")
	ctx := context.Background()
	db := newTestDB(t)
	boom := errors.New("boom")
	if _, err := db.InsertUpstreamKey(ctx, sampleKey("fp"), func(int64) (crypto.Sealed, error) {
		return crypto.Sealed{}, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("InsertUpstreamKey() error = %v, want boom", err)
	}
	keys, err := db.ListUpstreamKeys(ctx)
	if err != nil || len(keys) != 0 {
		t.Fatalf("ListUpstreamKeys() = %d keys, %v; want 0", len(keys), err)
	}
}

// STO-08 / STO-09
func TestUpstreamKeyUpdates(t *testing.T) {
	t.Skip("TODO(T02)")
	ctx := context.Background()
	db := newTestDB(t)
	id, err := db.InsertUpstreamKey(ctx, sampleKey("fp"), func(int64) (crypto.Sealed, error) {
		return crypto.Sealed{KEKVersion: 1, WrappedDEK: []byte("w"), Ciphertext: []byte("c")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := 7
	if err := db.UpdateUpstreamKeyMeta(ctx, id, store.UpstreamKeyUpdate{Weight: &w}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUpstreamKeyState(ctx, id, store.KeyStateQuarantined, "401 twice"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetUpstreamKey(ctx, id)
	if got.Weight != 7 || got.Name != "k" || got.State != store.KeyStateQuarantined || got.StateReason != "401 twice" {
		t.Fatalf("after updates = %+v", got)
	}
	if err := db.SetUpstreamKeyState(ctx, id, "cooldown", ""); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("SetUpstreamKeyState(cooldown) error = %v, want ErrInvalidArgument", err)
	}
	if err := db.UpdateUpstreamKeyMeta(ctx, 999, store.UpstreamKeyUpdate{Weight: &w}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateUpstreamKeyMeta(missing) error = %v, want ErrNotFound", err)
	}
}
