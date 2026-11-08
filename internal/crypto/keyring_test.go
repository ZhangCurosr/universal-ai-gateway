package crypto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestKeyring(t *testing.T) *Keyring {
	t.Helper()
	kr, err := NewKeyring(bytes.Repeat([]byte{7}, MasterKeySize), 1)
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}
	return kr
}

func TestLoadOrCreateMasterKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "master.key")
	k1, err := LoadOrCreateMasterKey(path) // SEC-01
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	k2, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("second load returned a different key")
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMasterKey(path); err == nil { // SEC-02
		t.Fatal("load of 5-byte key: error = nil, want error")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	kr := newTestKeyring(t)
	aad := UpstreamKeyAAD(42, "pool-a", kr.Version())
	s1, err := kr.Seal([]byte("sk-secret"), aad)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	s2, _ := kr.Seal([]byte("sk-secret"), aad)
	if bytes.Equal(s1.Ciphertext, s2.Ciphertext) { // SEC-04
		t.Fatal("two Seal() calls produced identical ciphertext")
	}
	pt, err := kr.Open(s1, aad)
	if err != nil || string(pt) != "sk-secret" {
		t.Fatalf("Open() = %q, %v; want sk-secret, nil", pt, err)
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	kr := newTestKeyring(t)
	aad := UpstreamKeyAAD(1, "p", 1)
	s, _ := kr.Seal([]byte("sk-secret"), aad)

	// SEC-06：AAD 不同（模拟把密文挪到另一行）
	if _, err := kr.Open(s, UpstreamKeyAAD(2, "p", 1)); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open(other row aad) error = %v, want ErrDecrypt", err)
	}
	// SEC-06：密文被改动
	bad := s
	bad.Ciphertext = append([]byte(nil), s.Ciphertext...)
	bad.Ciphertext[len(bad.Ciphertext)-1] ^= 1
	if _, err := kr.Open(bad, aad); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open(tampered) error = %v, want ErrDecrypt", err)
	}
	// SEC-05
	ver := s
	ver.KEKVersion = 2
	if _, err := kr.Open(ver, aad); !errors.Is(err, ErrUnknownKEKVersion) {
		t.Errorf("Open(version 2) error = %v, want ErrUnknownKEKVersion", err)
	}
}

func TestPepperDiffersFromKEKAndIsStable(t *testing.T) {
	a := newTestKeyring(t)
	b := newTestKeyring(t)
	if !bytes.Equal(a.TokenPepper(), b.TokenPepper()) {
		t.Fatal("pepper not deterministic for the same master key")
	}
	if len(a.TokenPepper()) != 32 {
		t.Fatalf("pepper length = %d, want 32", len(a.TokenPepper()))
	}
}
