package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyFile        = "key"
	keyLength      = 32
	cryptMagic     = "SHAS1"
	cryptMagicLen  = 5
	cryptNonceSize = 12
	cryptTagSize   = 16
)

func (s *Store) keyPath() string { return filepath.Join(s.blobsPath(), keyFile) }

// loadKey returns the 32-byte repo encryption key, or nil if none is configured
// (in which case push/pull use plaintext — same behavior as before encryption
// support existed). Priority: SHASYNC_KEY env var (hex), then .blobs/key.
func (s *Store) loadKey() ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv("SHASYNC_KEY")); v != "" {
		return decodeHexKey(v)
	}
	b, err := os.ReadFile(s.keyPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeHexKey(strings.TrimSpace(string(b)))
}

func decodeHexKey(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("key is not valid hex: %w", err)
	}
	if len(b) != keyLength {
		return nil, fmt.Errorf("key must decode to %d bytes (got %d)", keyLength, len(b))
	}
	return b, nil
}

func generateKey() ([]byte, error) {
	b := make([]byte, keyLength)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// encryptBytes wraps plaintext with AES-256-GCM.
// Wire format: magic(5) || nonce(12) || ciphertext_with_tag.
// Buffers the whole plaintext in memory; acceptable for folder-sync-sized files.
func encryptBytes(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, cryptNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, cryptMagicLen+cryptNonceSize+len(plaintext)+cryptTagSize)
	out = append(out, cryptMagic...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, nil), nil
}

func decryptBytes(key, framed []byte) ([]byte, error) {
	if len(framed) < cryptMagicLen+cryptNonceSize+cryptTagSize {
		return nil, fmt.Errorf("ciphertext too short (%d bytes)", len(framed))
	}
	if string(framed[:cryptMagicLen]) != cryptMagic {
		return nil, fmt.Errorf("bad magic: not a shasync-encrypted blob (wrong key or unencrypted remote?)")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := framed[cryptMagicLen : cryptMagicLen+cryptNonceSize]
	ct := framed[cryptMagicLen+cryptNonceSize:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w (wrong key or tampered ciphertext)", err)
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLength {
		return nil, fmt.Errorf("key must be %d bytes (got %d)", keyLength, len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func verifyPlaintextSHA(plaintext []byte, wantSHA string) error {
	sum := sha256.Sum256(plaintext)
	got := hex.EncodeToString(sum[:])
	if got != wantSHA {
		return fmt.Errorf("sha mismatch: got %s, expected %s", got, wantSHA)
	}
	return nil
}
