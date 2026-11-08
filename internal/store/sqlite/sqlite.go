// Package sqlite 是 store.Store 的 SQLite 实现（spec/04）。
//
// 只有 cmd/gateway 可以导入本包（ARCH-02），其他包通过 store.Store 接口访问。
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrDataDirLocked 表示数据目录已被另一个网关进程占用。
var ErrDataDirLocked = errors.New("sqlite: data dir is locked by another gateway process")

// DB 实现 store.Store。
type DB struct {
	db  *sql.DB
	now func() time.Time // 测试可替换
}

var _ store.Store = (*DB)(nil)

// Open 打开（必要时创建）数据库并执行迁移（spec/04 §1.1、§2）。
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {
			"journal_mode(WAL)",
			"busy_timeout(5000)",
			"synchronous(NORMAL)",
			"foreign_keys(ON)",
		},
		"_txlock": {"immediate"},
	}.Encode()
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sdb.SetMaxOpenConns(4)
	sdb.SetMaxIdleConns(4)
	sdb.SetConnMaxLifetime(0)
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close() // 已有更重要的错误要返回
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	d := &DB{db: sdb, now: time.Now}
	if err := d.migrate(ctx); err != nil {
		_ = sdb.Close() // 已有更重要的错误要返回
		return nil, err
	}
	return d, nil
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.db.Close() }

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad version prefix", name)
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		ms = append(ms, migration{version: v, name: name, sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 0001.. without gaps, got %s", m.name)
		}
	}
	return ms, nil
}

func (d *DB) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := d.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(ms) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", current, len(ms))
	}
	for _, m := range ms[current:] {
		if err := d.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后 Rollback 为 no-op
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, d.now().UnixMilli()); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}

// isUniqueViolation 判断是否为唯一约束冲突。这是本仓库唯一允许检查驱动错误的地方（spec/04 §4.1）。
func isUniqueViolation(err error) bool {
	var se *msqlite.Error
	if errors.As(err, &se) {
		code := se.Code()
		return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isBusy 判断是否为 SQLITE_BUSY / SQLITE_LOCKED（含扩展错误码）。
func isBusy(err error) bool {
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	primary := se.Code() & 0xff
	return primary == sqlite3.SQLITE_BUSY || primary == sqlite3.SQLITE_LOCKED
}

// boolToInt / intToBool 用于 INTEGER 布尔列。
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func intToBool(v int64) bool { return v != 0 }

// fromMillis 把数据库中的 Unix 毫秒转换为 UTC 时间。
func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// DirLock 持有数据目录锁，Close 释放。
type DirLock struct {
	db   *sql.DB
	conn *sql.Conn
}

// AcquireDirLock 在 path 打开独立的 SQLite 锁文件并持有排他锁（spec/04 §1.3）。
// 锁已被其他进程持有时返回 ErrDataDirLocked。
func AcquireDirLock(ctx context.Context, path string) (*DirLock, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {"locking_mode(EXCLUSIVE)", "busy_timeout(0)"},
	}.Encode()
	ldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	ldb.SetMaxOpenConns(1)
	conn, err := ldb.Conn(ctx)
	if err != nil {
		_ = ldb.Close() // 已有更重要的错误要返回
		return nil, fmt.Errorf("lock conn: %w", err)
	}
	_, err = conn.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS gateway_lock (id INTEGER PRIMARY KEY, locked_at INTEGER NOT NULL);
		 INSERT OR REPLACE INTO gateway_lock (id, locked_at) VALUES (1, ?);`, time.Now().UnixMilli())
	if err != nil {
		_ = conn.Close() // 已有更重要的错误要返回
		_ = ldb.Close()
		if isBusy(err) {
			return nil, ErrDataDirLocked
		}
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return &DirLock{db: ldb, conn: conn}, nil
}

// Close 释放锁。
func (l *DirLock) Close() error {
	cerr := l.conn.Close()
	derr := l.db.Close()
	return errors.Join(cerr, derr)
}
