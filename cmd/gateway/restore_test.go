package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

func TestRestorePreservesCredentialsBillingRuntimeAndPending(t *testing.T) {
	f := newBackupFixture(t)
	var stdout, stderr bytes.Buffer
	getenv := func(string) string { return testBackupPassword }
	if code := realMainIO([]string{"backup", "--config", f.config, "--output", f.archive}, strings.NewReader(""), &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("backup CLI = %d", code)
	}
	data, master, cfg := filepath.Join(f.root, "restored"), filepath.Join(f.root, "restored.key"), filepath.Join(f.root, "restored.yaml")
	if code := realMainIO([]string{"restore", "--file", f.archive, "--data-dir", data, "--master-key-file", master, "--config-output", cfg}, strings.NewReader(""), &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("restore CLI = %d: %s", code, stderr.String())
	}
	if !bytes.Equal(fileBytes(t, master), f.masterBytes) || !bytes.Equal(fileBytes(t, cfg), fileBytes(t, f.config)) {
		t.Fatal("restore changed master key or original config")
	}
	if !strings.Contains(stdout.String(), "update server.data_dir") {
		t.Fatal("restore omitted configuration-path instruction")
	}
	for _, secret := range []string{f.secret, f.token, testBackupPassword} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("CLI exposed a credential")
		}
	}
	db, err := sqlite.Open(t.Context(), filepath.Join(data, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // Restored test database cleanup.
	kr, err := crypto.NewKeyring(fileBytes(t, master), 1)
	if err != nil {
		t.Fatal(err)
	}
	client, err := db.GetClient(t.Context(), f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if client.TokenHash != auth.HashToken(kr.TokenPepper(), f.token) {
		t.Fatal("restored token no longer authenticates")
	}
	key, err := db.GetUpstreamKey(t.Context(), f.keyID)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := kr.Open(key.Sealed, crypto.UpstreamKeyAAD(key.ID, key.Pool, key.Sealed.KEKVersion))
	if err != nil || string(secret) != f.secret {
		t.Fatal("restored upstream key failed decryption")
	}
	keys, pins, err := db.LoadRuntimeState(t.Context(), time.Now())
	if err != nil || len(keys) != 1 || len(pins) != 1 || keys[0].AuthFailures != 1 || pins[0].KeyID != f.keyID {
		t.Fatal("restored runtime state differs")
	}
	if err := ledger.RecoverUnflushed(t.Context(), db, filepath.Join(data, "unflushed_usage.jsonl")); err != nil {
		t.Fatal(err)
	}
	costs, err := db.SumCostByClient(t.Context())
	if err != nil || costs[f.clientID] != 8 {
		t.Fatalf("restored settled plus pending cost = %d, err %v, want 8", costs[f.clientID], err)
	}
}

func TestBackupEnvelopeRejectsTamperingBeforePublication(t *testing.T) {
	blob, err := sealBackup([]byte("plain"), testBackupPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"magic", "iterations", "oversize", "salt", "nonce", "tag", "truncated", "trailing", "password", "short-password"} {
		t.Run(name, func(t *testing.T) {
			bad := append([]byte(nil), blob...)
			password := testBackupPassword
			switch name {
			case "magic":
				bad[0] ^= 1
			case "iterations":
				binary.BigEndian.PutUint32(bad[8:12], 0xffffffff)
			case "oversize":
				binary.BigEndian.PutUint64(bad[12:20], uint64(backupMaxBytes+1))
			case "salt":
				bad[20] ^= 1
			case "nonce":
				bad[36] ^= 1
			case "tag":
				bad[len(bad)-1] ^= 1
			case "truncated":
				bad = bad[:len(bad)-1]
			case "trailing":
				bad = append(bad, 0)
			case "password":
				password = "another-local-password"
			case "short-password":
				password = "short"
			}
			if _, err := openBackup(bad, password); err == nil {
				t.Fatal("invalid encrypted archive accepted")
			}
		})
	}
}

func rewriteTestTar(t *testing.T, plain []byte, change func(*tar.Header, []byte) (*tar.Header, []byte), extra bool) []byte {
	t.Helper()
	var out bytes.Buffer
	reader, writer := tar.NewReader(bytes.NewReader(plain)), tar.NewWriter(&out)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(io.LimitReader(reader, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		header, data = change(header, data)
		if header == nil {
			continue
		}
		header.Size = int64(len(data))
		if header.Typeflag == tar.TypeSymlink {
			header.Size = 0
			data = nil
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
		if extra && header.Name == "manifest.json" {
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRestoreRejectsArchiveMembersAndManifest(t *testing.T) {
	f := newBackupFixture(t)
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err != nil {
		t.Fatal(err)
	}
	plain, err := openBackup(fileBytes(t, f.archive), testBackupPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"traversal", "symlink", "unknown", "duplicate", "missing", "hash", "size", "version", "schema", "master-size", "config", "garbage", "truncated"} {
		t.Run(name, func(t *testing.T) {
			bad := rewriteTestTar(t, plain, func(h *tar.Header, b []byte) (*tar.Header, []byte) {
				if h.Name == "gateway.db" {
					switch name {
					case "traversal":
						h.Name = "../escape"
					case "unknown":
						h.Name = "unexpected"
					case "symlink":
						h.Typeflag, h.Linkname = tar.TypeSymlink, "/outside"
					case "missing":
						return nil, nil
					}
				}
				if h.Name == "manifest.json" {
					var m backupManifest
					if err := json.Unmarshal(b, &m); err != nil {
						t.Fatal(err)
					}
					switch name {
					case "hash":
						e := m.Files["gateway.db"]
						e.SHA256 = strings.Repeat("0", 64)
						m.Files["gateway.db"] = e
					case "size":
						e := m.Files["gateway.db"]
						e.Size++
						m.Files["gateway.db"] = e
					case "version":
						m.Version++
					case "schema":
						m.Schema = backupSchemaVersion + 1
					}
					var err error
					b, err = json.Marshal(m)
					if err != nil {
						t.Fatal(err)
					}
				}
				if (name == "master-size" && h.Name == "master.key") || (name == "config" && h.Name == "gateway.yaml") {
					b = []byte("invalid")
				}
				return h, b
			}, name == "duplicate")
			if name == "garbage" {
				bad = append(bad, 'x')
			}
			if name == "truncated" {
				bad = bad[:len(bad)-1024]
			}
			if _, _, err := unpackBackup(bad); err == nil {
				t.Fatal("malformed archive accepted")
			}
		})
	}
}

func TestRestoreRefusesExistingAndConcurrentTargets(t *testing.T) {
	f := newBackupFixture(t)
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err != nil {
		t.Fatal(err)
	}
	data, master, cfg := filepath.Join(f.root, "new-data"), filepath.Join(f.root, "new.key"), filepath.Join(f.root, "new.yaml")
	if err := os.WriteFile(master, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(t.Context(), f.archive, data, master, cfg, testBackupPassword); err == nil {
		t.Fatal("restore overwrote master")
	}
	if string(fileBytes(t, master)) != "existing" {
		t.Fatal("existing master changed")
	}
	if err := os.Remove(master); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- restoreBackup(t.Context(), f.archive, data, master, cfg, testBackupPassword)
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent restores succeeded %d times, want 1", successes)
	}
	if err := restoreBackup(t.Context(), f.archive, data, master, cfg, testBackupPassword); err == nil {
		t.Fatal("restore accepted existing data directory")
	}
}

func TestRestorePublicationRollbackAndCancellation(t *testing.T) {
	root := t.TempDir()
	data, master, cfg := filepath.Join(root, "data"), filepath.Join(root, "key"), filepath.Join(root, "config")
	if err := os.WriteFile(master, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"gateway.db": []byte("staged"), "master.key": []byte("new"), "gateway.yaml": []byte("config")}
	if err := publishRestore(t.Context(), data, master, cfg, files); err == nil {
		t.Fatal("publication overwrote existing key")
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publication left data directory: %v", err)
	}
	if string(fileBytes(t, master)) != "existing" {
		t.Fatal("rollback changed existing key")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := publishRestore(ctx, data, master, cfg, files); err == nil {
		t.Fatal("cancelled publication succeeded")
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled publication left directory: %v", err)
	}
}

func TestBackupRejectsSymlinksAndNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Skip("symlink creation unavailable on this host")
	}
	if _, err := readBackupFile(link, 1024); err == nil {
		t.Fatal("symlink source accepted")
	}
	if _, err := readBackupFile(root, 1024); err == nil {
		t.Fatal("directory source accepted")
	}
	if _, err := checkedPath(filepath.Join(link, "child"), false); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if err := publishBackupFile(filepath.Join(path, "output"), []byte("x")); err == nil {
		t.Fatal("publication into non-directory succeeded")
	}
}

func TestRestoreCLIValidationAndNoPartialData(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := cmdRestore(t.Context(), nil, func(string) string { return "" }, &out, &stderr); code != 2 {
		t.Fatalf("missing restore args = %d", code)
	}
	if code := cmdRestore(t.Context(), []string{"--bad"}, func(string) string { return "" }, &out, &stderr); code != 2 {
		t.Fatalf("unknown restore flag = %d", code)
	}
	f := newBackupFixture(t)
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(f.root, "destination")
	if code := cmdRestore(t.Context(), []string{"--file", f.archive, "--data-dir", data, "--master-key-file", filepath.Join(f.root, "key"), "--config-output", filepath.Join(f.root, "config")}, func(string) string { return "wrong-local-password" }, &out, &stderr); code != 1 {
		t.Fatalf("wrong-password restore = %d", code)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrong password created destination")
	}
	if strings.Contains(out.String()+stderr.String(), testBackupPassword) || strings.Contains(out.String()+stderr.String(), "wrong-local-password") {
		t.Fatal("restore printed password")
	}
}

func TestRestoreRejectsCorruptDatabaseBeforeCreatingTargets(t *testing.T) {
	f := newBackupFixture(t)
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err != nil {
		t.Fatal(err)
	}
	plain, err := openBackup(fileBytes(t, f.archive), testBackupPassword)
	if err != nil {
		t.Fatal(err)
	}
	files, manifest, err := unpackBackup(plain)
	if err != nil {
		t.Fatal(err)
	}
	files["gateway.db"] = []byte("not a sqlite database")
	bad, err := packBackup(files, manifest.Schema, backupMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealBackup(bad, testBackupPassword)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(f.root, "corrupt.uaig")
	if err := os.WriteFile(archive, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(f.root, "rejected-data")
	if err := restoreBackup(t.Context(), archive, data, filepath.Join(f.root, "rejected.key"), filepath.Join(f.root, "rejected.yaml"), testBackupPassword); err == nil {
		t.Fatal("corrupt authenticated database accepted")
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt database created destination")
	}
	leftovers, err := filepath.Glob(filepath.Join(f.root, ".uaig-restore-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("restore temporary files left: %v, %v", leftovers, err)
	}
}

func TestRestoreRejectsAliasedOutputsAndOversizeFile(t *testing.T) {
	f := newBackupFixture(t)
	if err := createBackup(t.Context(), f.config, f.archive, testBackupPassword, backupMaxBytes); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(f.root, "aliased-output")
	if err := restoreBackup(t.Context(), f.archive, filepath.Join(f.root, "data-new"), shared, shared, testBackupPassword); err == nil {
		t.Fatal("aliased outputs accepted")
	}
	oversize := filepath.Join(t.TempDir(), "oversize.uaig")
	file, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(backupMaxBytes + backupHeaderSize + 17); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(t.Context(), oversize, shared, shared+".key", shared+".yaml", testBackupPassword); err == nil {
		t.Fatal("oversize encrypted file accepted")
	}
}
