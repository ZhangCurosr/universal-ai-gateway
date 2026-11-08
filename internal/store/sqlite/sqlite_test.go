package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestDB 打开一个位于临时目录的数据库，并把时钟固定为 fixedNow。
func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db.now = func() time.Time { return fixedNow }
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var fixedNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestOpenRunsMigrations(t *testing.T) {
	db := newTestDB(t)
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	ms, _ := loadMigrations()
	if n != len(ms) {
		t.Fatalf("applied %d migrations, want %d", n, len(ms))
	}
	for _, table := range []string{"clients", "upstream_keys", "usage_events", "audit_records", "system_settings"} {
		var name string
		err := db.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		db, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open() #%d error = %v", i+1, err)
		}
		_ = db.Close()
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (999, 0)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("Open() of newer schema: error = nil, want error")
	}
}

func TestAcquireDirLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	ctx := context.Background()
	l1, err := AcquireDirLock(ctx, path)
	if err != nil {
		t.Fatalf("first AcquireDirLock() error = %v", err)
	}
	if _, err := AcquireDirLock(ctx, path); !errors.Is(err, ErrDataDirLocked) {
		t.Fatalf("second AcquireDirLock() error = %v, want ErrDataDirLocked", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	l3, err := AcquireDirLock(ctx, path)
	if err != nil {
		t.Fatalf("AcquireDirLock() after release error = %v", err)
	}
	_ = l3.Close()
}
