package main

// Keyframe + delta manifests on the remote.
//
// Locally every manifest is stored in full. On the remote, manifests/<sha>
// is either the full manifest or, when that's much smaller, a delta against
// a "keyframe": a manifest whose remote object is full. A delta never refers
// to another delta, so reading one takes at most one extra download. The
// object's name is still the SHA of the full manifest, and a downloaded
// delta is rebuilt and checked against it before it's stored.
//
// Delta plaintext (compact JSON, before zstd + encryption), always starting
// with `{"delta":1,`:
//
//	{"delta":1,"base":"<keyframe sha>","parent_sha":"…","created_at":123,
//	 "message":"…","set":{"path":{"sha":"…","size":1,…}},"del":["path"]}
//
// .blobs/keyframes lists, one per line, manifests known to be full on the
// remote: uploaded full from here, downloaded full, or named as a delta's base.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	deltaPrefix = `{"delta":`
	// Below this compressed size a full manifest is cheap enough to always send.
	deltaMinFull = 16 * 1024
	// A delta must be smaller than a tenth of the full manifest, and this.
	deltaMaxSize = 64 * 1024
	// How far up the parent chain to look for a keyframe.
	deltaMaxWalk  = 1000
	keyframesFile = "keyframes"
	deltaVersion  = 1
)

type manifestDelta struct {
	Delta     int                      `json:"delta"`
	Base      string                   `json:"base"`
	ParentSHA string                   `json:"parent_sha"`
	CreatedAt int64                    `json:"created_at"`
	Message   string                   `json:"message"`
	Set       map[string]*ManifestFile `json:"set"`
	Del       []string                 `json:"del"`
}

var keyframesMu sync.Mutex

func (s *Store) keyframesPath() string { return filepath.Join(s.blobsPath(), keyframesFile) }

func (s *Store) readKeyframes() map[string]struct{} {
	keyframesMu.Lock()
	defer keyframesMu.Unlock()
	out := make(map[string]struct{})
	f, err := os.Open(s.keyframesPath())
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out[l] = struct{}{}
		}
	}
	return out
}

// addKeyframe records that sha's remote object is a full manifest.
func (s *Store) addKeyframe(sha string) error {
	if _, ok := s.readKeyframes()[sha]; ok {
		return nil
	}
	keyframesMu.Lock()
	defer keyframesMu.Unlock()
	f, err := os.OpenFile(s.keyframesPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(sha + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func manifestCanonical(m *Manifest) ([]byte, string, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, "", err
	}
	b = append(b, '\n')
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

func sameManifestFile(a, b *ManifestFile) bool {
	return a.SHA == b.SHA && a.Size == b.Size && a.UpdatedAt == b.UpdatedAt && a.Mode == b.Mode
}

// makeDelta describes m as changes to base.
func makeDelta(baseSHA string, base, m *Manifest) *manifestDelta {
	d := &manifestDelta{Delta: deltaVersion, Base: baseSHA, ParentSHA: m.ParentSHA, CreatedAt: m.CreatedAt,
		Message: m.Message, Set: map[string]*ManifestFile{}, Del: []string{}}
	for p, f := range m.Files {
		if bf, ok := base.Files[p]; !ok || !sameManifestFile(bf, f) {
			d.Set[p] = f
		}
	}
	for p := range base.Files {
		if _, ok := m.Files[p]; !ok {
			d.Del = append(d.Del, p)
		}
	}
	sort.Strings(d.Del)
	return d
}

// applyDelta rebuilds the full manifest from base and d.
func applyDelta(base *Manifest, d *manifestDelta) *Manifest {
	files := make(map[string]*ManifestFile, len(base.Files)+len(d.Set))
	for p, f := range base.Files {
		files[p] = f
	}
	for _, p := range d.Del {
		delete(files, p)
	}
	for p, f := range d.Set {
		files[p] = f
	}
	if len(files) == 0 {
		files = nil
	}
	return &Manifest{Version: ManifestSchemaVersion, ParentSHA: d.ParentSHA, CreatedAt: d.CreatedAt, Message: d.Message, Files: files}
}

func encodeDelta(d *manifestDelta) ([]byte, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, []byte(`{"delta":1,`)) {
		return nil, fmt.Errorf("delta encoding: unexpected prefix")
	}
	return b, nil
}

func parseDelta(b []byte) (*manifestDelta, error) {
	var d manifestDelta
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("parse manifest delta: %w", err)
	}
	if d.Delta != deltaVersion {
		return nil, fmt.Errorf("unsupported manifest delta version %d", d.Delta)
	}
	if d.Base == "" {
		return nil, fmt.Errorf("manifest delta has no base")
	}
	return &d, nil
}

func isDeltaObject(plain []byte) bool { return bytes.HasPrefix(plain, []byte(deltaPrefix)) }

// manifestUploadForm returns the plaintext to upload as manifests/<sha>, and
// whether it's the full manifest (a keyframe).
func manifestUploadForm(s *Store, sha string) ([]byte, bool, error) {
	full, err := os.ReadFile(s.manifestPath(sha))
	if err != nil {
		return nil, false, err
	}
	m, err := parseManifest(full, sha)
	if err != nil {
		return nil, false, err
	}
	keyframes := s.readKeyframes()
	base := ""
	for p, n := m.ParentSHA, 0; p != "" && n < deltaMaxWalk && s.hasManifest(p); n++ {
		if _, ok := keyframes[p]; ok {
			base = p
			break
		}
		pm, err := s.readManifest(p)
		if err != nil {
			break
		}
		p = pm.ParentSHA
	}
	if base == "" {
		return full, true, nil
	}
	fullC, err := compressZstd(full)
	if err != nil {
		return nil, false, err
	}
	if len(fullC) < deltaMinFull {
		return full, true, nil
	}
	bm, err := s.readManifest(base)
	if err != nil {
		return full, true, nil
	}
	d := makeDelta(base, bm, m)
	db, err := encodeDelta(d)
	if err != nil {
		return nil, false, err
	}
	dc, err := compressZstd(db)
	if err != nil {
		return nil, false, err
	}
	limit := len(fullC) / 10
	if limit > deltaMaxSize {
		limit = deltaMaxSize
	}
	if len(dc) >= limit {
		return full, true, nil
	}
	// Only send what's known to rebuild exactly.
	back, err := parseDelta(db)
	if err != nil {
		return full, true, nil
	}
	if _, got, err := manifestCanonical(applyDelta(bm, back)); err != nil || got != sha {
		return full, true, nil
	}
	return db, false, nil
}

// uploadManifest uploads manifests/<sha>, as a delta against a keyframe when
// that's much smaller, else in full (and it becomes a keyframe).
func uploadManifest(ctx context.Context, r Remote, s *Store, cryptKey []byte, sha string) error {
	data, full, err := manifestUploadForm(s, sha)
	if err != nil {
		return err
	}
	if err := uploadPlaintext(ctx, r, cryptKey, data, remoteManifestKey(sha)); err != nil {
		return err
	}
	if full {
		return s.addKeyframe(sha)
	}
	return nil
}

// downloadManifestObject fetches manifests/<sha>, rebuilding it from its
// keyframe if it's a delta, verifies the SHA and stores it.
func downloadManifestObject(ctx context.Context, r Remote, s *Store, cryptKey []byte, sha string) error {
	key := remoteManifestKey(sha)
	plain, err := downloadPlaintext(ctx, r, cryptKey, key)
	if err != nil {
		return err
	}
	if isDeltaObject(plain) {
		d, err := parseDelta(plain)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if !s.hasManifest(d.Base) {
			bp, err := downloadPlaintext(ctx, r, cryptKey, remoteManifestKey(d.Base))
			if err != nil {
				return fmt.Errorf("%s: base %s: %w", key, d.Base, err)
			}
			if isDeltaObject(bp) {
				return fmt.Errorf("%s: base %s is itself a delta", key, d.Base)
			}
			if err := verifyPlaintextSHA(bp, d.Base); err != nil {
				return fmt.Errorf("%s: base: %w", key, err)
			}
			if err := os.MkdirAll(filepath.Dir(s.manifestPath(d.Base)), 0o755); err != nil {
				return err
			}
			if err := writeFileAtomic(s.manifestPath(d.Base), bp, 0o444); err != nil {
				return err
			}
		}
		bm, err := s.readManifest(d.Base)
		if err != nil {
			return err
		}
		rebuilt, got, err := manifestCanonical(applyDelta(bm, d))
		if err != nil {
			return err
		}
		if got != sha {
			return fmt.Errorf("%s: delta rebuilds to %s", key, got)
		}
		plain = rebuilt
		if err := s.addKeyframe(d.Base); err != nil {
			return err
		}
	} else {
		if err := verifyPlaintextSHA(plain, sha); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if err := s.addKeyframe(sha); err != nil {
			return err
		}
	}
	dst := s.manifestPath(sha)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(dst, plain, 0o444)
}
