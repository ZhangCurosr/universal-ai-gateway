package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func sampleKey(fp string) *store.UpstreamKey {
	return &store.UpstreamKey{Pool: "p1", Name: "k", Fingerprint: fp, SecretHint: "sk-a****wxyz", Weight: 1, MaxInFlight: 4, State: store.KeyStateReady}
}

// STO-06：seal 必须拿到真实自增 id，密文写入该行
func TestInsertUpstreamKeySealUsesID(t *testing.T) {
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

func TestGetUpstreamKeyNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, err := db.GetUpstreamKey(ctx, 999)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetUpstreamKey(999) error = %v, want ErrNotFound", err)
	}
}

func TestSetUpstreamKeyStateNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	err := db.SetUpstreamKeyState(ctx, 999, store.KeyStateReady, "")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetUpstreamKeyState(999) error = %v, want ErrNotFound", err)
	}
}

func TestUpdateUpstreamKeyMetaAllFields(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id, err := db.InsertUpstreamKey(ctx, sampleKey("fp2"), func(int64) (crypto.Sealed, error) {
		return crypto.Sealed{KEKVersion: 1, WrappedDEK: []byte("w"), Ciphertext: []byte("c")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	newName := "renamed"
	newMaxInFlight := 32
	if err := db.UpdateUpstreamKeyMeta(ctx, id, store.UpstreamKeyUpdate{
		Name:        &newName,
		MaxInFlight: &newMaxInFlight,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetUpstreamKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.MaxInFlight != 32 {
		t.Fatalf("got name=%s max_in_flight=%d, want renamed, 32", got.Name, got.MaxInFlight)
	}
}

// T62：过期时间与标签的插入、读取、更新与清除。
func TestUpstreamKeyExpiryAndLabel(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seal := func(int64) (crypto.Sealed, error) {
		return crypto.Sealed{KEKVersion: 1, WrappedDEK: []byte("w"), Ciphertext: []byte("c")}, nil
	}
	exp := time.Date(2026, 12, 1, 16, 0, 0, 0, time.UTC)
	k := sampleKey("fp-exp")
	k.ExpiresAt = exp.In(time.FixedZone("UTC+8", 8*3600))
	k.Label = "signup-2026-11"
	id, err := db.InsertUpstreamKey(ctx, k, seal)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetUpstreamKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(exp) || got.Label != "signup-2026-11" {
		t.Fatalf("inserted = %v %q", got.ExpiresAt, got.Label)
	}

	later, label := exp.Add(30*24*time.Hour), "renewed"
	if err := db.UpdateUpstreamKeyMeta(ctx, id, store.UpstreamKeyUpdate{ExpiresAt: &later, Label: &label}); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetUpstreamKey(ctx, id)
	if !got.ExpiresAt.Equal(later) || got.Label != "renewed" || got.Weight != 1 {
		t.Fatalf("after update = %+v", got)
	}

	var zero time.Time
	if err := db.UpdateUpstreamKeyMeta(ctx, id, store.UpstreamKeyUpdate{ExpiresAt: &zero}); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetUpstreamKey(ctx, id)
	if !got.ExpiresAt.IsZero() || got.Label != "renewed" {
		t.Fatalf("after clearing expiry = %v %q", got.ExpiresAt, got.Label)
	}
}

// T62：迁移前写入的行（不带新列）取默认值：不过期、无标签。
func TestUpstreamKeyMetadataDefaults(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.db.ExecContext(ctx, `INSERT INTO upstream_keys
		(pool, name, fingerprint, secret_hint, kek_version, wrapped_dek, ciphertext, created_at, updated_at)
		VALUES ('p1', 'old', 'fp-old', 'h', 1, x'00', x'00', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	keys, err := db.ListUpstreamKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].ExpiresAt.IsZero() || keys[0].Label != "" {
		t.Fatalf("legacy row = %+v", keys)
	}
}

// STO-18、STO-19：运行时状态整体替换，读取时过滤已结束的冷却与过期的固定记录
func TestRuntimeState(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	now := fixedNow
	keys := []store.KeyRuntime{
		{KeyID: 1, CooldownUntil: now.Add(time.Hour)},
		{KeyID: 2, CooldownUntil: now.Add(-time.Minute)}, // 已结束
		{KeyID: 3, AuthFailures: 1},
		{KeyID: 4, CooldownUntil: now.Add(-time.Minute), AuthFailures: 1},
	}
	pins := []store.TaskPin{
		{AffinityKey: "c1:j:aaaa", KeyID: 1, ExpiresAt: now.Add(48 * time.Hour)},
		{AffinityKey: "c1:j:bbbb", KeyID: 2, ExpiresAt: now.Add(-time.Second)},
	}
	if err := db.SaveRuntimeState(ctx, keys, pins); err != nil {
		t.Fatal(err)
	}
	gotK, gotP, err := db.LoadRuntimeState(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotK) != 3 || gotK[0].KeyID != 1 || !gotK[0].CooldownUntil.Equal(now.Add(time.Hour)) ||
		gotK[1].KeyID != 3 || !gotK[1].CooldownUntil.IsZero() || gotK[1].AuthFailures != 1 ||
		gotK[2].KeyID != 4 || !gotK[2].CooldownUntil.IsZero() {
		t.Fatalf("keys = %+v", gotK)
	}
	if len(gotP) != 1 || gotP[0].AffinityKey != "c1:j:aaaa" || gotP[0].KeyID != 1 || !gotP[0].ExpiresAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("pins = %+v", gotP)
	}
	// 整体替换：第二次保存覆盖第一次；空快照清空
	if err := db.SaveRuntimeState(ctx, []store.KeyRuntime{{KeyID: 9, AuthFailures: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	gotK, gotP, _ = db.LoadRuntimeState(ctx, now)
	if len(gotK) != 1 || gotK[0].KeyID != 9 || len(gotP) != 0 {
		t.Fatalf("after replace = %+v %+v", gotK, gotP)
	}
	if err := db.SaveRuntimeState(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if gotK, gotP, _ = db.LoadRuntimeState(ctx, now); len(gotK)+len(gotP) != 0 {
		t.Fatalf("after clear = %+v %+v", gotK, gotP)
	}
}
