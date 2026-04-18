package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

// --- Repo key file and wire-format constants ------------------------------

const (
	keyFile   = "key"
	keyLength = 32 // AES-256

	// Current wire format: chunked AES-256-GCM ("SHAS2").
	cryptMagicV2     = "SHAS2"
	cryptMagicLen    = 5
	cryptHeaderV2Len = 25 // magic(5) + chunk_size(4) + plaintext_size(8) + nonce_prefix(8)
	cryptNoncePrefix = 8  // v2
	cryptTagSize     = 16
	cryptChunkSize   = 65536 // 64 KiB plaintext per chunk — enables HTTP range reads on the ciphertext

	// Legacy whole-file format ("SHAS1") — accepted on read for backwards
	// compatibility with blobs pushed by older versions. We never write it.
	cryptMagicV1     = "SHAS1"
	cryptV1HeaderLen = 17 // magic(5) + 12-byte nonce

	// Argon2id parameters. Tuned for ~100ms on a modern laptop; keep stable
	// across all clients of a repo (they're not stored in the wire format).
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB → 64 MiB
	argonThreads = 4

	saltLength = 16
	saltFile   = "salt" // relative to repo's remote prefix
)

// Wire format v2 (bucket-side object bytes):
//
//   [0..5)   magic "SHAS2"
//   [5..9)   chunk_size (uint32 BE; plaintext bytes per chunk except last)
//   [9..17)  plaintext_size (uint64 BE)
//   [17..25) nonce_prefix (8 random bytes)
//   [25..)   concatenated chunks; chunk i is
//            AES-256-GCM(plaintext_chunk_i, nonce = nonce_prefix || uint32_BE(i))
//
// Chunk i occupies the ciphertext byte range
//   [25 + i*(chunk_size+16), 25 + (i+1)*(chunk_size+16))
// except the last chunk, which is (plaintext_size mod chunk_size) + 16 bytes.
// This layout lets a range-aware reader (e.g. a browser) fetch just the
// chunks overlapping a plaintext offset window.

// --- Store key plumbing ---------------------------------------------------

func (s *Store) keyPath() string { return filepath.Join(s.blobsPath(), keyFile) }

// loadKey returns the 32-byte repo encryption key, or nil if none is configured
// (in which case push/pull use plaintext). Priority:
//
//  1. SHASYNC_KEY env var (hex)
//  2. .blobs/key file (hex)
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

// --- Passphrase → key derivation -----------------------------------------

func generateSalt() ([]byte, error) {
	b := make([]byte, saltLength)
	_, err := rand.Read(b)
	return b, err
}

func deriveKeyFromPassphrase(passphrase, salt []byte) []byte {
	return argon2.IDKey(passphrase, salt, argonTime, argonMemory, argonThreads, keyLength)
}

// readPassphrase returns SHASYNC_PASSPHRASE if set, else prompts on the TTY
// without echoing. Returns an error if stdin isn't a terminal and no env var.
func readPassphrase(prompt string) ([]byte, error) {
	if v := os.Getenv("SHASYNC_PASSPHRASE"); v != "" {
		return []byte(v), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("stdin is not a terminal — set SHASYNC_PASSPHRASE instead")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return b, err
}

// --- Remote salt -----------------------------------------------------------

// remoteSaltKey is the remote-prefix-relative key where the per-repo Argon2id
// salt is stored. The salt is plaintext (16 random bytes); knowing it does not
// help an attacker without also knowing the passphrase.
func remoteSaltKey() string { return saltFile }

// fetchOrCreateSalt fetches <prefix>/salt from the remote, creating and
// uploading a fresh random salt if none exists yet.
func fetchOrCreateSalt(ctx context.Context, r Remote) ([]byte, bool, error) {
	ok, err := r.Exists(ctx, remoteSaltKey())
	if err != nil {
		return nil, false, err
	}
	if ok {
		rc, err := r.Download(ctx, remoteSaltKey())
		if err != nil {
			return nil, false, err
		}
		defer rc.Close()
		salt, err := io.ReadAll(rc)
		if err != nil {
			return nil, false, err
		}
		if len(salt) != saltLength {
			return nil, false, fmt.Errorf("remote salt has wrong length: %d bytes", len(salt))
		}
		return salt, false, nil
	}
	salt, err := generateSalt()
	if err != nil {
		return nil, false, err
	}
	if err := r.Upload(ctx, remoteSaltKey(), bytes.NewReader(salt)); err != nil {
		return nil, false, err
	}
	return salt, true, nil
}

// --- AES-256-GCM chunked encryption (v2) ----------------------------------

func encryptBytes(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	noncePrefix := make([]byte, cryptNoncePrefix)
	if _, err := rand.Read(noncePrefix); err != nil {
		return nil, err
	}

	numChunks := (len(plaintext) + cryptChunkSize - 1) / cryptChunkSize

	out := make([]byte, 0, cryptHeaderV2Len+len(plaintext)+numChunks*cryptTagSize)
	out = append(out, cryptMagicV2...)
	out = binary.BigEndian.AppendUint32(out, uint32(cryptChunkSize))
	out = binary.BigEndian.AppendUint64(out, uint64(len(plaintext)))
	out = append(out, noncePrefix...)

	nonce := make([]byte, 12)
	copy(nonce, noncePrefix)
	for i := 0; i < numChunks; i++ {
		lo := i * cryptChunkSize
		hi := lo + cryptChunkSize
		if hi > len(plaintext) {
			hi = len(plaintext)
		}
		binary.BigEndian.PutUint32(nonce[cryptNoncePrefix:], uint32(i))
		out = gcm.Seal(out, nonce, plaintext[lo:hi], nil)
	}
	return out, nil
}

func decryptBytes(key, framed []byte) ([]byte, error) {
	if len(framed) < cryptMagicLen {
		return nil, fmt.Errorf("ciphertext too short to contain magic")
	}
	switch string(framed[:cryptMagicLen]) {
	case cryptMagicV2:
		return decryptV2(key, framed)
	case cryptMagicV1:
		return decryptV1(key, framed)
	default:
		return nil, fmt.Errorf("unrecognized wire format (wrong key, unencrypted, or future version?)")
	}
}

func decryptV2(key, framed []byte) ([]byte, error) {
	if len(framed) < cryptHeaderV2Len {
		return nil, fmt.Errorf("v2 ciphertext too short")
	}
	chunkSize := binary.BigEndian.Uint32(framed[5:9])
	plaintextSize := binary.BigEndian.Uint64(framed[9:17])
	noncePrefix := framed[17:25]
	body := framed[cryptHeaderV2Len:]

	if chunkSize == 0 {
		return nil, fmt.Errorf("v2: invalid chunk_size 0")
	}
	numChunks := uint64(0)
	if plaintextSize > 0 {
		numChunks = (plaintextSize + uint64(chunkSize) - 1) / uint64(chunkSize)
	}
	expectedBody := plaintextSize + numChunks*uint64(cryptTagSize)
	if uint64(len(body)) != expectedBody {
		return nil, fmt.Errorf("v2: ciphertext body length mismatch (got %d, expected %d)", len(body), expectedBody)
	}

	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, 0, plaintextSize)
	nonce := make([]byte, 12)
	copy(nonce, noncePrefix)
	var pos uint64
	for i := uint64(0); i < numChunks; i++ {
		binary.BigEndian.PutUint32(nonce[cryptNoncePrefix:], uint32(i))
		var ctLen uint64
		if i == numChunks-1 {
			lastPT := plaintextSize - i*uint64(chunkSize)
			ctLen = lastPT + uint64(cryptTagSize)
		} else {
			ctLen = uint64(chunkSize) + uint64(cryptTagSize)
		}
		chunk := body[pos : pos+ctLen]
		pos += ctLen
		newPt, err := gcm.Open(nil, nonce, chunk, nil)
		if err != nil {
			return nil, fmt.Errorf("decrypt chunk %d: %w", i, err)
		}
		pt = append(pt, newPt...)
	}
	return pt, nil
}

// decryptV1 is the legacy whole-file scheme kept for backwards compatibility
// with any blobs pushed by earlier shasync versions. Writes always use v2.
func decryptV1(key, framed []byte) ([]byte, error) {
	if len(framed) < cryptV1HeaderLen+cryptTagSize {
		return nil, fmt.Errorf("v1 ciphertext too short")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := framed[cryptMagicLen : cryptMagicLen+12]
	ct := framed[cryptV1HeaderLen:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("v1 decrypt: %w", err)
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
