package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

func deltaStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := initStore(dir); err != nil {
		t.Fatal(err)
	}
	return &Store{Root: dir}
}

func fakeSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// bigFiles makes n file entries: incompressible SHAs, so the full manifest is
// well over deltaMinFull compressed.
func bigFiles(n int) map[string]*ManifestFile {
	files := make(map[string]*ManifestFile, n)
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("notes/note %04d.md", i)] = &ManifestFile{SHA: fakeSHA(fmt.Sprint(i)), Size: int64(100 + i), UpdatedAt: 1700000000000 + int64(i), Mode: 0o644}
	}
	return files
}

func cloneFiles(f map[string]*ManifestFile) map[string]*ManifestFile {
	out := make(map[string]*ManifestFile, len(f))
	for k, v := range f {
		c := *v
		out[k] = &c
	}
	return out
}

func writeM(t *testing.T, s *Store, parent string, files map[string]*ManifestFile, msg string) string {
	t.Helper()
	sha, _, err := s.writeManifest(&Manifest{Version: ManifestSchemaVersion, ParentSHA: parent, CreatedAt: 1700000000000, Message: msg, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func remotePlain(t *testing.T, r *memRemote, sha string) []byte {
	t.Helper()
	b, err := downloadPlaintext(context.Background(), r, testKey(t), remoteManifestKey(sha))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestManifestDeltaRoundTrip(t *testing.T) {
	ctx := context.Background()
	key := testKey(t)
	a := deltaStore(t)
	r := &memRemote{objs: map[string][]byte{}}

	base := bigFiles(3000)
	k := writeM(t, a, "", base, "first")
	if err := uploadManifest(ctx, r, a, key, k); err != nil {
		t.Fatal(err)
	}
	if isDeltaObject(remotePlain(t, r, k)) {
		t.Fatal("first manifest should be uploaded full")
	}

	// Small change: one edit, one add, one delete.
	f2 := cloneFiles(base)
	f2["notes/note 0007.md"].SHA = fakeSHA("edited")
	f2["notes/new \"quoted\" <tag> & ünïcode.md"] = &ManifestFile{SHA: fakeSHA("new"), Size: 5}
	delete(f2, "notes/note 0100.md")
	m2 := writeM(t, a, k, f2, "sync from abc")
	if err := uploadManifest(ctx, r, a, key, m2); err != nil {
		t.Fatal(err)
	}
	p2 := remotePlain(t, r, m2)
	if !strings.HasPrefix(string(p2), `{"delta":1,`) {
		t.Fatalf("small change should be a delta, got %.60s", p2)
	}
	t.Logf("delta: %s", p2)

	// Second small change: still a delta against the same keyframe (no chain).
	f3 := cloneFiles(f2)
	f3["notes/note 0008.md"].Size = 999
	m3 := writeM(t, a, m2, f3, "")
	if err := uploadManifest(ctx, r, a, key, m3); err != nil {
		t.Fatal(err)
	}
	d3, err := parseDelta(remotePlain(t, r, m3))
	if err != nil {
		t.Fatal(err)
	}
	if d3.Base != k {
		t.Fatalf("delta base = %s, want keyframe %s", d3.Base, k)
	}

	// Another machine downloads m3 without having anything: the base comes too.
	b := deltaStore(t)
	if err := downloadManifest(ctx, r, b, key, m3); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(b.manifestPath(m3))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(a.manifestPath(m3))
	if string(got) != string(want) {
		t.Fatal("rebuilt manifest differs")
	}
	if _, ok := b.readKeyframes()[k]; !ok {
		t.Fatal("downloaded delta's base should be a keyframe")
	}

	// Big change: full again, and it becomes the new keyframe.
	f4 := bigFiles(3000)
	for p, f := range f4 {
		f.SHA = fakeSHA("v2 " + p)
	}
	m4 := writeM(t, a, m3, f4, "")
	if err := uploadManifest(ctx, r, a, key, m4); err != nil {
		t.Fatal(err)
	}
	if isDeltaObject(remotePlain(t, r, m4)) {
		t.Fatal("big change should be uploaded full")
	}
	if _, ok := a.readKeyframes()[m4]; !ok {
		t.Fatal("full upload should become a keyframe")
	}
	f5 := cloneFiles(f4)
	f5["x.md"] = &ManifestFile{SHA: fakeSHA("x"), Size: 1}
	m5 := writeM(t, a, m4, f5, "")
	if err := uploadManifest(ctx, r, a, key, m5); err != nil {
		t.Fatal(err)
	}
	if d5, err := parseDelta(remotePlain(t, r, m5)); err != nil || d5.Base != m4 {
		t.Fatalf("next delta should use the new keyframe: %v %+v", err, d5)
	}
}

func TestManifestDeltaSmallManifestIsFull(t *testing.T) {
	ctx := context.Background()
	key := testKey(t)
	a := deltaStore(t)
	r := &memRemote{objs: map[string][]byte{}}
	k := writeM(t, a, "", bigFiles(10), "")
	if err := uploadManifest(ctx, r, a, key, k); err != nil {
		t.Fatal(err)
	}
	f := bigFiles(10)
	f["y.md"] = &ManifestFile{SHA: fakeSHA("y"), Size: 1}
	m := writeM(t, a, k, f, "")
	if err := uploadManifest(ctx, r, a, key, m); err != nil {
		t.Fatal(err)
	}
	if isDeltaObject(remotePlain(t, r, m)) {
		t.Fatal("a small manifest is always sent full")
	}
}

func TestManifestDeltaRejectsBadDeltas(t *testing.T) {
	ctx := context.Background()
	key := testKey(t)
	a := deltaStore(t)
	r := &memRemote{objs: map[string][]byte{}}
	k := writeM(t, a, "", bigFiles(3000), "")
	if err := uploadManifest(ctx, r, a, key, k); err != nil {
		t.Fatal(err)
	}
	f2 := bigFiles(3000)
	f2["z.md"] = &ManifestFile{SHA: fakeSHA("z"), Size: 1}
	m2 := writeM(t, a, k, f2, "")
	if err := uploadManifest(ctx, r, a, key, m2); err != nil {
		t.Fatal(err)
	}

	// A delta whose base is itself a delta.
	d := &manifestDelta{Delta: 1, Base: m2, Set: map[string]*ManifestFile{}, Del: []string{}}
	db, _ := encodeDelta(d)
	const fake = "1111111111111111111111111111111111111111111111111111111111111111"
	if err := uploadPlaintext(ctx, r, key, db, remoteManifestKey(fake)); err != nil {
		t.Fatal(err)
	}
	if err := downloadManifest(ctx, r, deltaStore(t), key, fake); err == nil || !strings.Contains(err.Error(), "itself a delta") {
		t.Fatalf("want base-is-a-delta error, got %v", err)
	}

	// A delta that rebuilds to the wrong SHA.
	d.Base = k
	db, _ = encodeDelta(d)
	if err := uploadPlaintext(ctx, r, key, db, remoteManifestKey(fake)); err != nil {
		t.Fatal(err)
	}
	if err := downloadManifest(ctx, r, deltaStore(t), key, fake); err == nil || !strings.Contains(err.Error(), "rebuilds to") {
		t.Fatalf("want SHA mismatch, got %v", err)
	}
}
