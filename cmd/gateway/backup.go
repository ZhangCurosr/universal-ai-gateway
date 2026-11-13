package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

const (
	backupMaxBytes      = int64(256 << 20)
	backupHeaderSize    = 48
	backupIterations    = 600000
	backupMagic         = "UAIGBK01"
	backupSchemaVersion = 3 // Update when a new database migration is introduced.
)

var errBackupFormat = errors.New("invalid or unsupported backup")

type backupEntry struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type backupManifest struct {
	Version int                    `json:"version"`
	Schema  int                    `json:"schema"`
	Files   map[string]backupEntry `json:"files"`
}

func cmdBackup(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "gateway.yaml", "source config")
	out := fs.String("output", "", "new encrypted archive")
	limit := fs.Int64("max-bytes", backupMaxBytes, "maximum plaintext archive size")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	password := getenv("UAIG_BACKUP_PASSWORD")
	if fs.NArg() != 0 || *out == "" || *limit < 1 || *limit > backupMaxBytes || len(password) < 16 {
		fmt.Fprintln(stderr, "gateway: backup requires --output, a 1..268435456 byte limit and UAIG_BACKUP_PASSWORD of at least 16 bytes")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := createBackup(ctx, *cfgPath, *out, password, *limit); err != nil {
		// Underlying filesystem/config/SQL errors can contain user-supplied values.
		fmt.Fprintln(stderr, "gateway: backup failed; stop the gateway and verify source files, output path and size limit")
		return 1
	}
	fmt.Fprintln(stdout, "backup created (encrypted)")
	return 0
}

// checkedPath rejects symlinks in both leaf and ancestor components.
func checkedPath(path string, mustExist bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) || p != abs || mustExist {
				return "", err
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			return "", errBackupFormat
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return abs, nil
}

func readBackupFile(path string, limit int64) ([]byte, error) {
	abs, err := checkedPath(path, true)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errBackupFormat
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close() // Read-only cleanup; the read/stat error takes precedence.
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errBackupFormat
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBackupFormat
	}
	return data, nil
}

func backupDB(ctx context.Context, path string) (*sql.DB, error) {
	abs, err := checkedPath(path, true)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func validateBackupDB(ctx context.Context, db *sql.DB) (int, error) {
	var schema int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&schema); err != nil {
		return 0, err
	}
	if schema < 1 || schema > backupSchemaVersion {
		return 0, errBackupFormat
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return 0, err
	}
	if integrity != "ok" {
		return 0, errBackupFormat
	}
	return schema, nil
}

func createBackup(ctx context.Context, cfgPath, output, password string, limit int64) error {
	if limit < 1 || limit > backupMaxBytes || len(password) < 16 {
		return errBackupFormat
	}
	cfgBytes, err := readBackupFile(cfgPath, min(limit, 1<<20))
	if err != nil {
		return err
	}
	cfg, err := config.Parse(cfgBytes)
	if err != nil {
		return err
	}
	dataDir, err := checkedPath(cfg.Server.DataDir, true)
	if err != nil {
		return err
	}
	lockPath, err := checkedPath(filepath.Join(dataDir, "gateway.lock"), false)
	if err != nil {
		return err
	}
	lock, err := sqlite.AcquireDirLock(ctx, lockPath)
	if err != nil {
		return err
	}
	defer lock.Close() // Cleanup of the offline lock; operation errors take precedence.
	master, err := readBackupFile(cfg.Server.MasterKeyFile, crypto.MasterKeySize)
	if err != nil {
		return err
	}
	if len(master) != crypto.MasterKeySize {
		return errBackupFormat
	}
	out, err := checkedPath(output, false)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return os.ErrExist
	}
	// BK-02: Include WAL-backed pages through SQLite, without migrating/checkpointing the source.
	path := filepath.Join(dataDir, "gateway.db")
	var sourceBytes int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p, err := checkedPath(path+suffix, false)
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errBackupFormat
		}
		sourceBytes += info.Size()
		if sourceBytes > limit {
			return errBackupFormat
		}
	}
	db, err := backupDB(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close() // Read-only source connection cleanup.
	schema, err := validateBackupDB(ctx, db)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(out), ".uaig-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage) // Private scratch files are removed on every exit.
	snapshot := filepath.Join(stage, "gateway.db")
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return err
	}
	if err := os.Chmod(snapshot, 0o600); err != nil {
		return err
	}
	body, err := readBackupFile(snapshot, limit)
	if err != nil {
		return err
	}
	files := map[string][]byte{"gateway.db": body, "gateway.yaml": cfgBytes, "master.key": master}
	unflushed, err := readBackupFile(filepath.Join(dataDir, "unflushed_usage.jsonl"), limit)
	if err == nil {
		files["unflushed_usage.jsonl"] = unflushed
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	plain, err := packBackup(files, schema, limit)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sealed, err := sealBackup(plain, password)
	if err != nil {
		return err
	}
	return publishBackupFile(out, sealed)
}

func packBackup(files map[string][]byte, schema int, limit int64) ([]byte, error) {
	manifest := backupManifest{Version: 1, Schema: schema, Files: make(map[string]backupEntry)}
	names := make([]string, 0, len(files))
	var size int64 = 1024
	for name, data := range files {
		sum := sha256.Sum256(data)
		manifest.Files[name] = backupEntry{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
		names = append(names, name)
		size += 512 + (int64(len(data))+511)/512*512
	}
	meta, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	size += 512 + (int64(len(meta))+511)/512*512
	if size > limit {
		return nil, errBackupFormat
	}
	sort.Strings(names)
	var buf bytes.Buffer
	buf.Grow(int(size))
	tw := tar.NewWriter(&buf)
	for _, name := range append([]string{"manifest.json"}, names...) {
		data := files[name]
		if name == "manifest.json" {
			data = meta
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > limit {
		return nil, errBackupFormat
	}
	return buf.Bytes(), nil
}

func backupAEAD(password string, salt []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, backupIterations, 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealBackup(plain []byte, password string) ([]byte, error) {
	header := make([]byte, backupHeaderSize)
	copy(header, backupMagic)
	binary.BigEndian.PutUint32(header[8:12], backupIterations)
	binary.BigEndian.PutUint64(header[12:20], uint64(len(plain)))
	if _, err := rand.Read(header[20:]); err != nil {
		return nil, err
	}
	aead, err := backupAEAD(password, header[20:36])
	if err != nil {
		return nil, err
	}
	return aead.Seal(header, header[36:48], plain, header), nil
}

// publishBackupFile syncs a private file then links it exclusively; Rename could overwrite.
func publishBackupFile(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".uaig-publish-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // Scratch link cleanup, including failed publication.
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Link(f.Name(), path)
}
