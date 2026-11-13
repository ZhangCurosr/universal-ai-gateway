package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

const testBackupPassword = "local-test-backup-password"

type backupFixture struct {
	root, data, config, master, archive string
	masterBytes                         []byte
	token, secret                       string
	clientID, keyID                     int64
}

func newBackupFixture(t *testing.T) backupFixture {
	t.Helper()
	root := t.TempDir()
	f := backupFixture{root: root, data: filepath.Join(root, "data"), config: filepath.Join(root, "gateway.yaml"), master: filepath.Join(root, "master.key"), archive: filepath.Join(root, "backup.uaig"), masterBytes: bytes.Repeat([]byte{17}, 32), secret: "synthetic-backup-upstream-secret"}
	if err := os.Mkdir(f.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.master, f.masterBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("server:\n  data_dir: %q\n  master_key_file: %q\npools:\n  - name: local\n    protocol: rest\n    base_url: http://127.0.0.1:1\nmodels:\n  - name: local\n    pool: local\n    price: {requests: \"0.01\"}\n    reserve: {requests: 1}\n", filepath.ToSlash(f.data), filepath.ToSlash(f.master))
	if err := os.WriteFile(f.config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(t.Context(), filepath.Join(f.data, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	kr, err := crypto.NewKeyring(f.masterBytes, 1)
	if err != nil {
		t.Fatal(err)
	}
	token, display, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	f.token = token
	f.clientID, err = db.CreateClient(t.Context(), &store.Client{Name: "backup-client", Role: store.RoleAdmin, TokenHash: auth.HashToken(kr.TokenPepper(), token), TokenDisplay: display, QuotaUnlimited: true})
	if err != nil {
		t.Fatal(err)
	}
	f.keyID, err = db.InsertUpstreamKey(t.Context(), &store.UpstreamKey{Pool: "local", Name: "backup-key", Fingerprint: "fixture", Weight: 5, MaxInFlight: 4, State: store.KeyStateReady}, func(id int64) (crypto.Sealed, error) {
		return kr.Seal([]byte(f.secret), crypto.UpstreamKeyAAD(id, "local", 1))
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := store.UsageEvent{RequestID: "persisted-backup-event", ClientID: f.clientID, KeyID: f.keyID, Model: "local", Op: "call", Usage: pricing.Usage{"requests": 1}, CostNanoUSD: 1, UsageSource: "upstream", CreatedAt: time.Now().UTC()}
	if err := db.InsertUsageEvents(t.Context(), []store.UsageEvent{ev}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRuntimeState(t.Context(), []store.KeyRuntime{{KeyID: f.keyID, CooldownUntil: time.Now().Add(time.Hour), AuthFailures: 1}}, []store.TaskPin{{AffinityKey: "c1:j:0123456789abcdef", KeyID: f.keyID, ExpiresAt: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ev.RequestID, ev.CostNanoUSD = "pending-backup-event", 7
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.data, "unflushed_usage.jsonl"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackupRoundTripAndSourceUnchanged(t *testing.T) {
	f := newBackupFixture(t)
	paths := []string{f.config, f.master, filepath.Join(f.data, "gateway.db"), filepath.Join(f.data, "unflushed_usage.jsonl")}
	before := make(map[string][32]byte)
	for _, p := range paths {
		before[p] = sha256.Sum256(fileBytes(t, p))
	}
	var out, stderr bytes.Buffer
	getenv := func(string) string { return testBackupPassword }
	if code := cmdBackup(t.Context(), []string{"--config", f.config, "--output", f.archive}, getenv, &out, &stderr); code != 0 {
		t.Fatalf("cmdBackup = %d, stderr %q", code, stderr.String())
	}
	for _, p := range paths {
		if sha256.Sum256(fileBytes(t, p)) != before[p] {
			t.Errorf("backup changed source %s", filepath.Base(p))
		}
	}
	sealed := fileBytes(t, f.archive)
	for _, secret := range []string{f.token, f.secret, testBackupPassword} {
		if bytes.Contains(sealed, []byte(secret)) || strings.Contains(out.String()+stderr.String(), secret) {
			t.Fatal("backup exposed a credential")
		}
	}
	plain, err := openBackup(append([]byte(nil), sealed...), testBackupPassword)
	if err != nil {
		t.Fatal(err)
	}
	files, manifest, err := unpackBackup(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 || manifest.Schema != 3 || !bytes.Equal(files["master.key"], f.masterBytes) {
		t.Fatal("archive did not preserve required members")
	}
}

func TestBackupRejectsOnlineOversizeAndExistingOutput(t *testing.T) {
	f := newBackupFixture(t)
	lock, err := sqlite.AcquireDirLock(t.Context(), filepath.Join(f.data, "gateway.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err == nil {
		t.Fatal("backup while online succeeded")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{0, 1, backupMaxBytes + 1} {
		if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, limit); err == nil {
			t.Fatalf("backup limit %d succeeded", limit)
		}
	}
	if err := os.WriteFile(f.archive, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err == nil {
		t.Fatal("backup overwrote existing output")
	}
	if string(fileBytes(t, f.archive)) != "existing" {
		t.Fatal("existing output changed")
	}
}

func TestBackupIncludesWALWithoutChangingSchema(t *testing.T) {
	f := newBackupFixture(t)
	db, err := sqlite.Open(t.Context(), filepath.Join(f.data, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // Test fixture cleanup.
	if err := db.PutSetting(t.Context(), "wal-only", "present"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(f.data, "gateway.db-wal")); err != nil || info.Size() == 0 {
		t.Fatal("fixture did not retain WAL")
	}
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err != nil {
		t.Fatalf("WAL backup: %v", err)
	}
	plain, err := openBackup(fileBytes(t, f.archive), testBackupPassword)
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := unpackBackup(plain)
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(snap, files["gateway.db"], 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := backupDB(t.Context(), snap)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() // Read-only fixture cleanup.
	var value string
	if err := reader.QueryRowContext(t.Context(), "SELECT value FROM system_settings WHERE key = ?", "wal-only").Scan(&value); err != nil || value != "present" {
		t.Fatalf("snapshot WAL value = %q, err %v", value, err)
	}
}

func TestBackupCLIValidationAndRedaction(t *testing.T) {
	for _, args := range [][]string{{}, {"--output", "x", "extra"}, {"--output", "x", "--max-bytes", "-1"}, {"--bad"}} {
		var out, stderr bytes.Buffer
		if code := cmdBackup(context.Background(), args, func(string) string { return "" }, &out, &stderr); code != 2 {
			t.Fatalf("cmdBackup(%v) = %d, want 2", args, code)
		}
	}
	var out, stderr bytes.Buffer
	secret := "credential-in-invalid-path"
	if code := cmdBackup(t.Context(), []string{"--config", secret, "--output", "unused"}, func(string) string { return testBackupPassword }, &out, &stderr); code != 1 {
		t.Fatalf("cmdBackup missing source = %d, want 1", code)
	}
	if strings.Contains(out.String()+stderr.String(), secret) {
		t.Fatal("backup printed user-controlled error contents")
	}
}

func TestBackupSchemaVersionMatchesMigrations(t *testing.T) {
	entries, err := os.ReadDir("../../internal/store/sqlite/migrations")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			n++
		}
	}
	if n != backupSchemaVersion {
		t.Fatalf("backup supports schema %d, but %d migrations exist", backupSchemaVersion, n)
	}
}
