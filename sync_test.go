package main

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// --- conflictCopyPath ----------------------------------------------------

func TestConflictCopyPath(t *testing.T) {
	const cli = "a1b2c3d4"
	const stamp = "2026-04-20 101530"
	cases := []struct {
		in, want string
	}{
		{"notes/todo.md", "notes/todo (modified on 2026-04-20 101530 by a1b2c3d4).md"},
		{"hello.txt", "hello (modified on 2026-04-20 101530 by a1b2c3d4).txt"},
		{"README", "README (modified on 2026-04-20 101530 by a1b2c3d4)"},
		// Dotfile: leading dot is not treated as an extension separator
		// (path.LastIndex would pick it, but our code only splits when dot > 0).
		{".env", ".env (modified on 2026-04-20 101530 by a1b2c3d4)"},
		// Multi-dot filename: only the LAST dot is split.
		{"archive.tar.gz", "archive.tar (modified on 2026-04-20 101530 by a1b2c3d4).gz"},
		// Nested directory preserved.
		{"a/b/c/report.pdf", "a/b/c/report (modified on 2026-04-20 101530 by a1b2c3d4).pdf"},
		// Hidden file with extension: "dot > 0" → extension split still happens.
		{".config.json", ".config (modified on 2026-04-20 101530 by a1b2c3d4).json"},
	}
	for _, c := range cases {
		got := conflictCopyPath(c.in, cli, stamp)
		if got != c.want {
			t.Errorf("conflictCopyPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- autoMerge case matrix -----------------------------------------------

// Build a small in-memory store via a temp dir and write three manifests
// representing an ancestor, a remote-tip, and a local-tip. autoMerge is a
// pure function of those three manifests plus (hostname, now), so we can
// assert its behavior file-by-file without touching S3 or the working tree.
func mergeFixture(t *testing.T, anc, remote, local map[string]string) (*Store, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := initStore(dir); err != nil {
		t.Fatal(err)
	}
	s := &Store{Root: dir}

	mkFiles := func(m map[string]string) map[string]*ManifestFile {
		out := make(map[string]*ManifestFile, len(m))
		for p, sha := range m {
			// In the merge builder only identity by SHA matters. Size/mtime are
			// carried along but not inspected; use sentinel values.
			out[p] = &ManifestFile{SHA: sha, Size: int64(len(sha)), UpdatedAt: 0}
		}
		return out
	}
	writeMan := func(files map[string]string, parent string) string {
		sha, _, err := s.writeManifest(&Manifest{
			Version:   ManifestSchemaVersion,
			ParentSHA: parent,
			CreatedAt: 1,
			Files:     mkFiles(files),
		})
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	ancSHA := writeMan(anc, "")
	remSHA := writeMan(remote, ancSHA)
	locSHA := writeMan(local, ancSHA)
	return s, ancSHA, remSHA, locSHA
}

func TestAutoMerge_NoChanges(t *testing.T) {
	s, a, r, l := mergeFixture(t,
		map[string]string{"a": "sha-a", "b": "sha-b"},
		map[string]string{"a": "sha-a", "b": "sha-b"},
		map[string]string{"a": "sha-a", "b": "sha-b"},
	)
	m, conflicts, err := autoMerge(s, "h", time.Unix(0, 0), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if m.ParentSHA != r {
		t.Fatalf("parent = %s, want remote %s", m.ParentSHA, r)
	}
	if len(m.Files) != 2 {
		t.Fatalf("file count = %d, want 2", len(m.Files))
	}
}

func TestAutoMerge_DisjointChanges(t *testing.T) {
	// Remote adds r.txt, local adds l.txt, a.txt unchanged. No conflicts.
	s, a, r, l := mergeFixture(t,
		map[string]string{"a.txt": "sha-a"},
		map[string]string{"a.txt": "sha-a", "r.txt": "sha-r"},
		map[string]string{"a.txt": "sha-a", "l.txt": "sha-l"},
	)
	m, conflicts, err := autoMerge(s, "h", time.Unix(0, 0), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	paths := sortedKeys(m.Files)
	want := []string{"a.txt", "l.txt", "r.txt"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if m.Files["r.txt"].SHA != "sha-r" {
		t.Fatalf("r.txt SHA = %q, want sha-r", m.Files["r.txt"].SHA)
	}
	if m.Files["l.txt"].SHA != "sha-l" {
		t.Fatalf("l.txt SHA = %q, want sha-l", m.Files["l.txt"].SHA)
	}
}

func TestAutoMerge_SameChangeBothSides(t *testing.T) {
	// Both machines edited the same file to the same new content: no conflict.
	s, a, r, l := mergeFixture(t,
		map[string]string{"x": "old"},
		map[string]string{"x": "new"},
		map[string]string{"x": "new"},
	)
	m, conflicts, err := autoMerge(s, "h", time.Unix(0, 0), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if m.Files["x"].SHA != "new" {
		t.Fatalf("x SHA = %q, want new", m.Files["x"].SHA)
	}
	if len(m.Files) != 1 {
		t.Fatalf("file count = %d, want 1", len(m.Files))
	}
}

func TestAutoMerge_TrueConflict(t *testing.T) {
	// Both edited the same file to different contents → conflict.
	// Remote keeps the original path; local is stashed as a sibling.
	s, a, r, l := mergeFixture(t,
		map[string]string{"notes/todo.md": "old"},
		map[string]string{"notes/todo.md": "remote-new"},
		map[string]string{"notes/todo.md": "local-new"},
	)
	m, conflicts, err := autoMerge(s, "a1b2c3d4", mustTime("2026-04-20T10:15:30Z"), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conflicts, []string{"notes/todo.md"}) {
		t.Fatalf("conflicts = %v, want [notes/todo.md]", conflicts)
	}
	if m.Files["notes/todo.md"].SHA != "remote-new" {
		t.Fatalf("original path kept remote? got SHA %q, want remote-new", m.Files["notes/todo.md"].SHA)
	}
	copyPath := "notes/todo (modified on 2026-04-20 101530 by a1b2c3d4).md"
	if m.Files[copyPath] == nil {
		t.Fatalf("expected conflict-copy file at %q, got paths: %v", copyPath, sortedKeys(m.Files))
	}
	if m.Files[copyPath].SHA != "local-new" {
		t.Fatalf("conflict copy SHA = %q, want local-new", m.Files[copyPath].SHA)
	}
}

func TestAutoMerge_LocalDeletesRemoteModifies(t *testing.T) {
	// Local deleted, remote modified → conflict. Remote version kept at path;
	// local "deletion" manifests as no conflict-copy (no blob to preserve).
	s, a, r, l := mergeFixture(t,
		map[string]string{"gone.txt": "v1"},
		map[string]string{"gone.txt": "v2"},
		map[string]string{},
	)
	m, conflicts, err := autoMerge(s, "cli01", mustTime("2026-04-20T10:15:30Z"), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conflicts, []string{"gone.txt"}) {
		t.Fatalf("conflicts = %v, want [gone.txt]", conflicts)
	}
	if m.Files["gone.txt"].SHA != "v2" {
		t.Fatalf("gone.txt = %q, want v2 (remote kept)", m.Files["gone.txt"].SHA)
	}
	// No sibling copy because local had no version to stash.
	for p := range m.Files {
		if p != "gone.txt" {
			t.Fatalf("unexpected extra file %q in merge", p)
		}
	}
}

func TestAutoMerge_RemoteDeletesLocalModifies(t *testing.T) {
	// Remote deleted, local modified → conflict. Remote "wins" (deletion kept),
	// but local version is stashed as a sibling so work isn't lost.
	s, a, r, l := mergeFixture(t,
		map[string]string{"keep.txt": "v1"},
		map[string]string{},
		map[string]string{"keep.txt": "local-edit"},
	)
	m, conflicts, err := autoMerge(s, "cli01", mustTime("2026-04-20T10:15:30Z"), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conflicts, []string{"keep.txt"}) {
		t.Fatalf("conflicts = %v, want [keep.txt]", conflicts)
	}
	if _, ok := m.Files["keep.txt"]; ok {
		t.Fatalf("keep.txt present in merge; remote deleted it — should be gone")
	}
	copyPath := "keep (modified on 2026-04-20 101530 by cli01).txt"
	if m.Files[copyPath] == nil || m.Files[copyPath].SHA != "local-edit" {
		t.Fatalf("expected local version stashed at %q, got %v", copyPath, sortedKeys(m.Files))
	}
}

func TestAutoMerge_BothDelete(t *testing.T) {
	s, a, r, l := mergeFixture(t,
		map[string]string{"dead.txt": "v"},
		map[string]string{},
		map[string]string{},
	)
	m, conflicts, err := autoMerge(s, "h", time.Unix(0, 0), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("both-delete is not a conflict, got %v", conflicts)
	}
	if _, ok := m.Files["dead.txt"]; ok {
		t.Fatalf("dead.txt still present")
	}
}

func TestAutoMerge_BothAddSamePathDifferentContent(t *testing.T) {
	// Neither side had the file in the ancestor, but both added it with
	// different SHAs. This is a conflict.
	s, a, r, l := mergeFixture(t,
		map[string]string{},
		map[string]string{"new.txt": "r"},
		map[string]string{"new.txt": "l"},
	)
	m, conflicts, err := autoMerge(s, "cli01", mustTime("2026-04-20T10:15:30Z"), a, r, l)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conflicts, []string{"new.txt"}) {
		t.Fatalf("conflicts = %v, want [new.txt]", conflicts)
	}
	if m.Files["new.txt"].SHA != "r" {
		t.Fatalf("new.txt = %q, want r", m.Files["new.txt"].SHA)
	}
	copyPath := "new (modified on 2026-04-20 101530 by cli01).txt"
	if m.Files[copyPath] == nil || m.Files[copyPath].SHA != "l" {
		t.Fatalf("expected local copy at %q, got %v", copyPath, sortedKeys(m.Files))
	}
}

// --- ancestor queries ----------------------------------------------------

// chainFixture writes N manifests each parented on the previous, returns
// their SHAs tip-last.
func chainFixture(t *testing.T, n int) (*Store, []string) {
	t.Helper()
	dir := t.TempDir()
	if err := initStore(dir); err != nil {
		t.Fatal(err)
	}
	s := &Store{Root: dir}
	var out []string
	parent := ""
	for i := 0; i < n; i++ {
		sha, _, err := s.writeManifest(&Manifest{
			Version:   ManifestSchemaVersion,
			ParentSHA: parent,
			CreatedAt: int64(i + 1),
			Message:   "c",
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, sha)
		parent = sha
	}
	return s, out
}

func TestIsAncestor(t *testing.T) {
	s, chain := chainFixture(t, 4) // [c0, c1, c2, c3], c3 = tip
	c0, c1, c2, c3 := chain[0], chain[1], chain[2], chain[3]

	check := func(a, d string, want bool) {
		t.Helper()
		got, err := s.isAncestor(a, d)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("isAncestor(%s, %s) = %v, want %v", shortSHA(a), shortSHA(d), got, want)
		}
	}
	// Inclusive: every commit is its own ancestor.
	check(c3, c3, true)
	check(c0, c0, true)
	// Real ancestors.
	check(c0, c3, true)
	check(c1, c3, true)
	check(c2, c3, true)
	// Not ancestors (direction matters).
	check(c3, c0, false)
	check(c2, c1, false)
	// Empty-string edge cases: both must be non-empty.
	check("", c3, false)
	check(c0, "", false)
}

func TestIsAncestor_UnknownManifestReturnsFalseNotError(t *testing.T) {
	// Policy per sync.go: if the chain hits a manifest we don't have, callers
	// should get (false, nil) so they can treat it as "unknown — needs fetch".
	s, chain := chainFixture(t, 2)
	bogus := "ff" + chain[0][2:] // a SHA we've definitely never written
	got, err := s.isAncestor(bogus, chain[1])
	if err != nil {
		t.Fatalf("expected nil error for unknown ancestor, got %v", err)
	}
	if got {
		t.Fatal("expected false for unknown ancestor")
	}
}

func TestFindCommonAncestor(t *testing.T) {
	// Build a fork: shared base c0, then remote = c0 → r1 → r2, local = c0 → l1.
	dir := t.TempDir()
	if err := initStore(dir); err != nil {
		t.Fatal(err)
	}
	s := &Store{Root: dir}
	write := func(parent, msg string) string {
		sha, _, err := s.writeManifest(&Manifest{
			Version:   ManifestSchemaVersion,
			ParentSHA: parent,
			CreatedAt: 1,
			Message:   msg,
		})
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	c0 := write("", "base")
	r1 := write(c0, "r1")
	r2 := write(r1, "r2")
	l1 := write(c0, "l1")

	got, err := s.findCommonAncestor(l1, r2)
	if err != nil {
		t.Fatal(err)
	}
	if got != c0 {
		t.Fatalf("findCommonAncestor = %s, want %s", shortSHA(got), shortSHA(c0))
	}

	// Self-ancestor: findCommonAncestor(x, x) == x.
	if got, _ := s.findCommonAncestor(r2, r2); got != r2 {
		t.Fatalf("self-ancestor = %s, want %s", shortSHA(got), shortSHA(r2))
	}

	// Linear history (no fork): ancestor is the older one.
	if got, _ := s.findCommonAncestor(r1, r2); got != r1 {
		t.Fatalf("linear common ancestor = %s, want %s", shortSHA(got), shortSHA(r1))
	}
}

// --- helpers -------------------------------------------------------------

func sortedKeys(m map[string]*ManifestFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustTime(iso string) time.Time {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		panic(err)
	}
	return t
}
