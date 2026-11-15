package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

func cmdRestore(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "encrypted archive")
	dataDir := fs.String("data-dir", "", "new data directory")
	master := fs.String("master-key-file", "", "new master key file")
	cfg := fs.String("config-output", "", "new config file (original paths retained)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	password := getenv("UAIG_BACKUP_PASSWORD")
	if fs.NArg() != 0 || *file == "" || *dataDir == "" || *master == "" || *cfg == "" || len(password) < 16 {
		fmt.Fprintln(stderr, "gateway: restore requires --file, --data-dir, --master-key-file, --config-output and UAIG_BACKUP_PASSWORD of at least 16 bytes")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := restoreBackup(ctx, *file, *dataDir, *master, *cfg, password); err != nil {
		fmt.Fprintln(stderr, "gateway: restore failed; verify password, archive, new destination paths and available space")
		return 1
	}
	fmt.Fprintln(stdout, "restore complete; update server.data_dir and server.master_key_file in the restored config, then run check-config before starting")
	return 0
}

func openBackup(blob []byte, password string) ([]byte, error) {
	if len(password) < 16 || len(blob) < backupHeaderSize+16 || string(blob[:8]) != backupMagic {
		return nil, errBackupFormat
	}
	header := blob[:backupHeaderSize]
	n := binary.BigEndian.Uint64(header[12:20])
	if binary.BigEndian.Uint32(header[8:12]) != backupIterations || n > uint64(backupMaxBytes) || uint64(len(blob)) != uint64(backupHeaderSize+16)+n {
		return nil, errBackupFormat
	}
	aead, err := backupAEAD(password, header[20:36])
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(blob[backupHeaderSize:backupHeaderSize], header[36:48], blob[backupHeaderSize:], header)
	if err != nil {
		return nil, errBackupFormat
	}
	return plain, nil
}

func unpackBackup(plain []byte) (map[string][]byte, backupManifest, error) {
	var manifest backupManifest
	if int64(len(plain)) > backupMaxBytes {
		return nil, manifest, errBackupFormat
	}
	allowed := map[string]bool{"manifest.json": true, "gateway.db": true, "gateway.yaml": true, "master.key": true, "unflushed_usage.jsonl": true}
	files := make(map[string][]byte)
	reader := bytes.NewReader(plain)
	tr := tar.NewReader(reader)
	for {
		before := reader.Len()
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if before-reader.Len() < 1024 {
				return nil, manifest, errBackupFormat
			}
			break
		}
		if err != nil {
			return nil, manifest, errBackupFormat
		}
		if !allowed[h.Name] || h.Format != tar.FormatUSTAR || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA) || h.Size < 0 || h.Size > backupMaxBytes {
			return nil, manifest, errBackupFormat
		}
		if _, exists := files[h.Name]; exists {
			return nil, manifest, errBackupFormat
		}
		if h.Name == "manifest.json" && h.Size > 64<<10 {
			return nil, manifest, errBackupFormat
		}
		data, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil || int64(len(data)) != h.Size {
			return nil, manifest, errBackupFormat
		}
		files[h.Name] = data
	}
	// Reject data hidden after tar's end markers, as well as absent/truncated end markers.
	if len(plain) < 1024 || !bytes.Equal(plain[len(plain)-1024:], make([]byte, 1024)) {
		return nil, manifest, errBackupFormat
	}
	for i := len(plain) - reader.Len(); i < len(plain); i++ {
		if plain[i] != 0 {
			return nil, manifest, errBackupFormat
		}
	}
	meta, ok := files["manifest.json"]
	if !ok {
		return nil, manifest, errBackupFormat
	}
	dec := json.NewDecoder(bytes.NewReader(meta))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return nil, manifest, errBackupFormat
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, manifest, errBackupFormat
	}
	delete(files, "manifest.json")
	if manifest.Version != 1 || manifest.Schema < 1 || manifest.Schema > backupSchemaVersion || len(manifest.Files) != len(files) {
		return nil, manifest, errBackupFormat
	}
	for _, name := range []string{"gateway.db", "gateway.yaml", "master.key"} {
		if _, ok := files[name]; !ok {
			return nil, manifest, errBackupFormat
		}
	}
	for name, data := range files {
		entry, exists := manifest.Files[name]
		sum := sha256.Sum256(data)
		if !exists || entry.Size != int64(len(data)) || entry.SHA256 != hex.EncodeToString(sum[:]) {
			return nil, manifest, errBackupFormat
		}
	}
	if len(files["master.key"]) != crypto.MasterKeySize {
		return nil, manifest, errBackupFormat
	}
	if _, err := config.Parse(files["gateway.yaml"]); err != nil {
		return nil, manifest, errBackupFormat
	}
	return files, manifest, nil
}

func restoreBackup(ctx context.Context, file, dataDir, master, cfg, password string) error {
	blob, err := readBackupFile(file, backupMaxBytes+backupHeaderSize+16)
	if err != nil {
		return err
	}
	plain, err := openBackup(blob, password)
	if err != nil {
		return err
	}
	files, manifest, err := unpackBackup(plain)
	if err != nil {
		return err
	}
	targets := []*string{&dataDir, &master, &cfg}
	seen := make(map[string]bool)
	for _, target := range targets {
		abs, err := checkedPath(*target, false)
		if err != nil {
			return err
		}
		if seen[abs] {
			return errBackupFormat
		}
		seen[abs] = true
		if _, err := os.Lstat(abs); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return os.ErrExist
		}
		*target = abs
	}
	// Parents must already exist; a restore never creates or replaces their hierarchy.
	guard := dataDir + ".restore-lock"
	if err := os.Mkdir(guard, 0o700); err != nil {
		return err
	}
	defer os.Remove(guard) // This invocation owns the empty coordination directory.
	stage, err := os.MkdirTemp(filepath.Dir(dataDir), ".uaig-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage) // Private, validated scratch files only.
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o600); err != nil {
			return err
		}
	}
	db, err := backupDB(ctx, filepath.Join(stage, "gateway.db"))
	if err != nil {
		return err
	}
	schema, checkErr := validateBackupDB(ctx, db)
	closeErr := db.Close()
	if err := errors.Join(checkErr, closeErr); err != nil {
		return err
	}
	if schema != manifest.Schema {
		return errBackupFormat
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishRestore(ctx, dataDir, master, cfg, files)
}

func publishRestore(ctx context.Context, dataDir, master, cfg string, files map[string][]byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		return err
	}
	published := []string{}
	defer func() {
		if err == nil {
			return
		}
		// Only paths created by this invocation are removed. Existing destinations are untouched.
		for i := len(published) - 1; i >= 0; i-- {
			if cleanupErr := os.Remove(published[i]); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				err = errors.Join(err, cleanupErr)
			}
		}
		if cleanupErr := os.Remove(dataDir); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	lockPath := filepath.Join(dataDir, "gateway.lock")
	published = append(published, lockPath)
	lock, err := sqlite.AcquireDirLock(ctx, lockPath)
	if err != nil {
		return err
	}
	defer lock.Close() // Runs before rollback so Windows can remove the owned lock file.
	for _, entry := range []struct{ name, path string }{
		{"gateway.db", filepath.Join(dataDir, "gateway.db")},
		{"unflushed_usage.jsonl", filepath.Join(dataDir, "unflushed_usage.jsonl")},
		{"master.key", master}, {"gateway.yaml", cfg},
	} {
		data, ok := files[entry.name]
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := publishBackupFile(entry.path, data); err != nil {
			return err
		}
		published = append(published, entry.path)
	}
	return nil
}
