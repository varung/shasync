package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	blobsDir     = ".blobs"
	objectsDir   = "objects"
	manifestsDir = "manifests"
	headFile     = "HEAD"
	configFile   = "config.json"
	ignoreFile   = ".blobsignore"
)

// Store is a handle to a .blobs directory rooted at Root.
type Store struct {
	Root string // project root (the dir containing .blobs)
}

func findStore() (*Store, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir := cwd
	for {
		if st, err := os.Stat(filepath.Join(dir, blobsDir)); err == nil && st.IsDir() {
			return &Store{Root: dir}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("not inside a shasync project (no %s/ found)", blobsDir)
		}
		dir = parent
	}
}

func (s *Store) blobsPath() string     { return filepath.Join(s.Root, blobsDir) }
func (s *Store) objectsPath() string   { return filepath.Join(s.blobsPath(), objectsDir) }
func (s *Store) manifestsPath() string { return filepath.Join(s.blobsPath(), manifestsDir) }
func (s *Store) headPath() string      { return filepath.Join(s.blobsPath(), headFile) }
func (s *Store) configPath() string    { return filepath.Join(s.blobsPath(), configFile) }

func (s *Store) objectPath(sha string) string {
	// shard by first 2 chars to keep dirs shallow
	return filepath.Join(s.objectsPath(), sha[:2], sha[2:])
}
func (s *Store) manifestPath(sha string) string {
	return filepath.Join(s.manifestsPath(), sha[:2], sha[2:]+".json")
}

func initStore(root string) error {
	bp := filepath.Join(root, blobsDir)
	if _, err := os.Stat(bp); err == nil {
		return fmt.Errorf("%s/ already exists", blobsDir)
	}
	for _, d := range []string{bp, filepath.Join(bp, objectsDir), filepath.Join(bp, manifestsDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// --- HEAD ---

func (s *Store) readHead() (string, error) {
	b, err := os.ReadFile(s.headPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *Store) writeHead(sha string) error {
	return writeFileAtomic(s.headPath(), []byte(sha+"\n"), 0o644)
}

// --- Config ---

type Config struct {
	Remote string `json:"remote,omitempty"` // gs://bucket/prefix or s3://bucket/prefix
	// ClientID uniquely identifies this clone of the repo. Generated at init
	// time and persisted here; used to label conflict-copy files so you can
	// tell which machine produced the divergent version. Unlike hostname,
	// it survives laptop renames and doesn't collide across machines with
	// the same OS-level hostname.
	ClientID string `json:"client_id,omitempty"`
}

// newClientID returns 8 hex chars (32 bits of entropy) — enough for a
// single-user-multi-machine repo where only a handful of clones ever exist.
func newClientID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ensureClientID loads config and, if ClientID is empty (pre-existing repo
// or a hand-built config), generates one and writes it back. Returns the ID.
func (s *Store) ensureClientID() (string, error) {
	c, err := s.readConfig()
	if err != nil {
		return "", err
	}
	if c.ClientID != "" {
		return c.ClientID, nil
	}
	id, err := newClientID()
	if err != nil {
		return "", err
	}
	c.ClientID = id
	if err := s.writeConfig(c); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) readConfig() (*Config, error) {
	b, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) writeConfig(c *Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.configPath(), append(b, '\n'), 0o644)
}

// --- Objects ---

func (s *Store) hasObject(sha string) bool {
	_, err := os.Stat(s.objectPath(sha))
	return err == nil
}

// ingestFile hashes src, moves/copies it into the object store at its SHA
// (using reflink when possible so the working file and object share extents),
// and returns the SHA. If the object already exists, the file is not duplicated.
func (s *Store) ingestFile(src string) (string, int64, error) {
	sha, size, err := hashFile(src)
	if err != nil {
		return "", 0, err
	}
	dst := s.objectPath(sha)
	if _, err := os.Stat(dst); err == nil {
		return sha, size, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	if err := cloneOrCopyFile(src, dst); err != nil {
		return "", 0, err
	}
	// Objects are immutable — make them read-only to catch accidental writes.
	_ = os.Chmod(dst, 0o444)
	return sha, size, nil
}

// writeBytesAsObject writes the given bytes into the object store if missing.
func (s *Store) writeBytesAsObject(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	dst := s.objectPath(sha)
	if _, err := os.Stat(dst); err == nil {
		return sha, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := writeFileAtomic(dst, b, 0o444); err != nil {
		return "", err
	}
	return sha, nil
}

// --- Manifests ---

func (s *Store) writeManifest(m *Manifest) (string, []byte, error) {
	// canonical JSON: sorted keys via MarshalIndent with map — Go sorts map keys.
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", nil, err
	}
	b = append(b, '\n')
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	dst := s.manifestPath(sha)
	if _, err := os.Stat(dst); err != nil {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", nil, err
		}
		if err := writeFileAtomic(dst, b, 0o444); err != nil {
			return "", nil, err
		}
	}
	return sha, b, nil
}

func (s *Store) readManifest(sha string) (*Manifest, error) {
	b, err := os.ReadFile(s.manifestPath(sha))
	if err != nil {
		return nil, err
	}
	return parseManifest(b, sha)
}

// parseManifest decodes manifest bytes with strict schema enforcement:
// unknown top-level fields are rejected, and only recognized schema
// versions are accepted. This keeps a future v2 manifest from being
// silently misread by this v1 codebase.
func parseManifest(b []byte, sha string) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", sha, err)
	}
	if m.Version != ManifestSchemaVersion {
		return nil, fmt.Errorf("manifest %s: unsupported schema version %d (this build supports %d)", sha, m.Version, ManifestSchemaVersion)
	}
	if len(m.Chunks) != 0 {
		return nil, fmt.Errorf("manifest %s: chunks field populated but schema is v%d (reserved for future use)", sha, ManifestSchemaVersion)
	}
	return &m, nil
}

func (s *Store) hasManifest(sha string) bool {
	_, err := os.Stat(s.manifestPath(sha))
	return err == nil
}

// --- Helpers ---

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func nowUnixMs() int64 { return time.Now().UnixMilli() }
