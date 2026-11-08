package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func sampleClient(hash string) *store.Client {
	return &store.Client{
		Name:          "team-a",
		Role:          store.RoleUser,
		TokenHash:     hash,
		TokenDisplay:  "uai_live_abcdef…",
		QuotaNanoUSD:  1_000_000_000,
		RPMLimit:      60,
		AllowedModels: []string{"m1", "m2"},
	}
}

// STO-01 ~ STO-04
func TestClientsCRUD(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	id, err := db.CreateClient(ctx, sampleClient("h1"))
	if err != nil || id <= 0 {
		t.Fatalf("CreateClient() = %d, %v", id, err)
	}
	got, err := db.GetClient(ctx, id)
	if err != nil {
		t.Fatalf("GetClient() error = %v", err)
	}
	if got.Name != "team-a" || got.TokenHash != "h1" || !reflect.DeepEqual(got.AllowedModels, []string{"m1", "m2"}) {
		t.Fatalf("GetClient() = %+v", got)
	}
	if !got.CreatedAt.Equal(fixedNow) || !got.UpdatedAt.Equal(fixedNow) {
		t.Fatalf("timestamps = %v / %v, want %v", got.CreatedAt, got.UpdatedAt, fixedNow)
	}

	got.Disabled = true
	got.AllowedModels = nil
	if err := db.UpdateClient(ctx, got); err != nil {
		t.Fatalf("UpdateClient() error = %v", err)
	}
	again, _ := db.GetClient(ctx, id)
	if !again.Disabled || len(again.AllowedModels) != 0 {
		t.Fatalf("after update = %+v", again)
	}

	if _, err := db.CreateClient(ctx, sampleClient("h2")); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListClients(ctx)
	if err != nil || len(list) != 2 || list[0].ID >= list[1].ID {
		t.Fatalf("ListClients() = %+v, %v; want 2 clients ascending by id", list, err)
	}

	if _, err := db.GetClient(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetClient(missing) error = %v, want ErrNotFound", err)
	}
}

// STO-01：token_hash 唯一
func TestClientConflict(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.CreateClient(ctx, sampleClient("dup")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateClient(ctx, sampleClient("dup")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateClient(dup) error = %v, want ErrConflict", err)
	}
}

// STO-04：UpdateClient 不存在返回 ErrNotFound
func TestUpdateClientNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	c := sampleClient("missing")
	c.ID = 9999
	if err := db.UpdateClient(ctx, c); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateClient(missing) error = %v, want ErrNotFound", err)
	}
}

// STO-04：UpdateClient token_hash 冲突返回 ErrConflict
func TestUpdateClientConflict(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.CreateClient(ctx, sampleClient("h1")); err != nil {
		t.Fatal(err)
	}
	id2, err := db.CreateClient(ctx, sampleClient("h2"))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := db.GetClient(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	c2.TokenHash = "h1"
	if err := db.UpdateClient(ctx, c2); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("UpdateClient(dup token_hash) error = %v, want ErrConflict", err)
	}
}

// STO-03：空表返回空切片
func TestListClientsEmpty(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	list, err := db.ListClients(ctx)
	if err != nil {
		t.Fatalf("ListClients() error = %v", err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("ListClients() empty = %+v, want empty non-nil slice", list)
	}
}

// STO-05
func TestCountAdmins(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	a := sampleClient("a")
	a.Role = store.RoleAdmin
	b := sampleClient("b")
	b.Role = store.RoleAdmin
	b.Disabled = true
	for _, c := range []*store.Client{a, b, sampleClient("c")} {
		if _, err := db.CreateClient(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.CountAdmins(ctx); err != nil || n != 1 {
		t.Fatalf("CountAdmins() = %d, %v; want 1", n, err)
	}
}

// STO-15
func TestSettings(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if v, ok, err := db.GetSetting(ctx, "k"); err != nil || ok || v != "" {
		t.Fatalf("GetSetting(missing) = %q, %v, %v", v, ok, err)
	}
	for _, val := range []string{"v1", "v2"} {
		if err := db.PutSetting(ctx, "k", val); err != nil {
			t.Fatal(err)
		}
		if v, ok, err := db.GetSetting(ctx, "k"); err != nil || !ok || v != val {
			t.Fatalf("GetSetting() = %q, %v, %v; want %q", v, ok, err, val)
		}
	}
}
