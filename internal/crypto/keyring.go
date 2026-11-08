// Package crypto 实现主密钥管理、HKDF 子密钥派生与上游凭据的信封加密（spec/05 §2）。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// MasterKeySize 是主密钥字节数。
const MasterKeySize = 32

// HKDF info 常量是协议的一部分，永远不能修改（SEC 2.2）。
const (
	infoKEK         = "uaig/v1/kek"
	infoTokenPepper = "uaig/v1/token-pepper"
)

var (
	// ErrDecrypt 表示密文被篡改或 AAD 不匹配（SEC-06），不暴露具体原因。
	ErrDecrypt = errors.New("crypto: decryption failed")
	// ErrUnknownKEKVersion 表示密文使用了当前 Keyring 不认识的 KEK 版本（SEC-05）。
	ErrUnknownKEKVersion = errors.New("crypto: unknown kek version")
)

// LoadOrCreateMasterKey 读取主密钥文件；不存在时生成并以 0600 写入（SEC-01 ~ SEC-03）。
func LoadOrCreateMasterKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != MasterKeySize {
			return nil, fmt.Errorf("master key %s: want %d bytes, got %d", path, MasterKeySize, len(data))
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read master key: %w", err)
	}

	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create master key dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // 清理失败不影响返回的主错误
		return nil, fmt.Errorf("install master key: %w", err)
	}
	return key, nil
}

// Keyring 持有由主密钥派生的子密钥。并发安全（只读）。
type Keyring struct {
	version int
	kek     cipher.AEAD
	pepper  []byte
}

// NewKeyring 从 32 字节主密钥派生子密钥。version 写入每个 Sealed，用于将来轮换。
func NewKeyring(master []byte, version int) (*Keyring, error) {
	if len(master) != MasterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes", MasterKeySize)
	}
	if version < 1 {
		return nil, errors.New("kek version must be >= 1")
	}
	kekBytes, err := hkdf.Key(sha256.New, master, nil, infoKEK, 32)
	if err != nil {
		return nil, fmt.Errorf("derive kek: %w", err)
	}
	kek, err := newGCM(kekBytes)
	if err != nil {
		return nil, err
	}
	pepper, err := hkdf.Key(sha256.New, master, nil, infoTokenPepper, 32)
	if err != nil {
		return nil, fmt.Errorf("derive pepper: %w", err)
	}
	return &Keyring{version: version, kek: kek, pepper: pepper}, nil
}

// Version 返回当前 KEK 版本。
func (k *Keyring) Version() int { return k.version }

// TokenPepper 返回用于 Token / 指纹 HMAC 的 pepper 副本。
func (k *Keyring) TokenPepper() []byte {
	return append([]byte(nil), k.pepper...)
}

// Sealed 是一条信封加密后的记录。
type Sealed struct {
	KEKVersion int
	WrappedDEK []byte // nonce || AES-GCM(KEK, DEK, aad) || tag
	Ciphertext []byte // nonce || AES-GCM(DEK, plaintext, aad) || tag
}

// Seal 用新的随机 DEK 加密 plaintext，并用 KEK 包裹 DEK（SEC-04）。
func (k *Keyring) Seal(plaintext, aad []byte) (Sealed, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, fmt.Errorf("generate dek: %w", err)
	}
	dekAEAD, err := newGCM(dek)
	if err != nil {
		return Sealed{}, err
	}
	ct, err := sealWith(dekAEAD, plaintext, aad)
	if err != nil {
		return Sealed{}, err
	}
	wrapped, err := sealWith(k.kek, dek, aad)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{KEKVersion: k.version, WrappedDEK: wrapped, Ciphertext: ct}, nil
}

// Open 解开信封并返回明文（SEC-05、SEC-06）。
func (k *Keyring) Open(s Sealed, aad []byte) ([]byte, error) {
	if s.KEKVersion != k.version {
		return nil, ErrUnknownKEKVersion
	}
	dek, err := openWith(k.kek, s.WrappedDEK, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	dekAEAD, err := newGCM(dek)
	if err != nil {
		return nil, ErrDecrypt
	}
	pt, err := openWith(dekAEAD, s.Ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// UpstreamKeyAAD 生成上游 Key 行的 AAD（SEC-07）。其他地方禁止手写 AAD。
func UpstreamKeyAAD(id int64, pool string, kekVersion int) []byte {
	return []byte("uaig:v1:upstream_keys:" + strconv.FormatInt(id, 10) + ":" + pool + ":" + strconv.Itoa(kekVersion))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return gcm, nil
}

func sealWith(a cipher.AEAD, plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return a.Seal(nonce, nonce, plaintext, aad), nil
}

func openWith(a cipher.AEAD, data, aad []byte) ([]byte, error) {
	if len(data) < a.NonceSize()+a.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, ct := data[:a.NonceSize()], data[a.NonceSize():]
	return a.Open(nil, nonce, ct, aad)
}
