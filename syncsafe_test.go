package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// memRemote is an in-memory bucket. onBlobDownload runs once, at the first blob
// download — i.e. mid-sync, after the cycle has snapshotted the working tree.
type memRemote struct {
	mu             sync.Mutex
	objs           map[string][]byte
	onBlobDownload func()
}

func (m *memRemote) Exists(_ context.Context, k string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[k]
	return ok, nil
}

func (m *memRemote) Upload(_ context.Context, k string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[k] = b
	return nil
}

func (m *memRemote) Download(_ context.Context, k string) (io.ReadCloser, error) {
	if strings.HasPrefix(k, "blobs/") && m.onBlobDownload != nil {
		f := m.onBlobDownload
		m.onBlobDownload = nil
		f()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[k]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memRemote) List(_ context.Context, p string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, p) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

type machineT struct {
	s *Store
	c *Config
}

func newMachine(t *testing.T, id string) machineT {
	t.Helper()
	dir := t.TempDir()
	if err := initStore(dir); err != nil {
		t.Fatal(err)
	}
	return machineT{&Store{Root: dir}, &Config{ClientID: id}}
}

func (m machineT) put(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(m.s.Root, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (m machineT) get(name string) string {
	b, err := os.ReadFile(filepath.Join(m.s.Root, name))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// sync runs one watch cycle; edits during the cycle may make it stop early.
func (m machineT) sync(t *testing.T, r Remote) {
	t.Helper()
	if err := syncOnce(context.Background(), m.s, r, nil, m.c); err != nil && err != errLocalEditsDuringSync {
		t.Fatal(err)
	}
}

// twoMachines: B creates the repo, A clones it.
func twoMachines(t *testing.T, files map[string]string) (*memRemote, machineT, machineT) {
	t.Helper()
	r := &memRemote{objs: map[string][]byte{}}
	a, b := newMachine(t, "machineA"), newMachine(t, "machineB")
	for name, data := range files {
		b.put(t, name, data)
	}
	b.sync(t, r)
	head, err := readRemoteHead(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := fetchAndCheckout(context.Background(), r, a.s, nil, head); err != nil {
		t.Fatal(err)
	}
	return r, a, b
}

// Edits and new files saved while a sync is downloading must never be lost.
func TestWatchSyncKeepsEditsMadeDuringSync(t *testing.T) {
	r, a, b := twoMachines(t, map[string]string{"shared.md": "v1\n", "draft.md": "draft v1\n"})
	b.put(t, "shared.md", "v2 from B\n")
	b.sync(t, r)

	r.onBlobDownload = func() {
		a.put(t, "draft.md", "draft v2, typed on A during sync\n")
		a.put(t, "brand-new.md", "created on A during sync\n")
	}
	a.sync(t, r)

	if got := a.get("shared.md"); got != "v2 from B\n" {
		t.Errorf("B's change not applied on A: shared.md = %q", got)
	}
	if got := a.get("draft.md"); got != "draft v2, typed on A during sync\n" {
		t.Errorf("edit made during sync was overwritten: draft.md = %q", got)
	}
	if got := a.get("brand-new.md"); got != "created on A during sync\n" {
		t.Errorf("file created during sync was deleted: brand-new.md = %q", got)
	}

	// The next cycles carry A's edits to B.
	a.sync(t, r)
	b.sync(t, r)
	if got := b.get("draft.md"); got != "draft v2, typed on A during sync\n" {
		t.Errorf("A's edit never reached B: draft.md = %q", got)
	}
	if got := b.get("brand-new.md"); got != "created on A during sync\n" {
		t.Errorf("A's new file never reached B: brand-new.md = %q", got)
	}
}

// Editing the very file the remote changed, mid-sync: the remote version wins
// the path and the local edit is kept as a conflict copy — never lost.
func TestWatchSyncSameFileEditedDuringSyncBecomesConflictCopy(t *testing.T) {
	r, a, b := twoMachines(t, map[string]string{"note.md": "v1\n"})
	b.put(t, "note.md", "v2 from B\n")
	b.sync(t, r)

	r.onBlobDownload = func() { a.put(t, "note.md", "v2 typed on A during sync\n") }
	a.sync(t, r)
	if got := a.get("note.md"); got != "v2 typed on A during sync\n" {
		t.Fatalf("edit overwritten mid-sync: note.md = %q", got)
	}
	a.sync(t, r) // commits A's edit, merges with B's

	entries, _ := os.ReadDir(a.s.Root)
	var copies []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "note (modified on") {
			copies = append(copies, e.Name())
		}
	}
	if got := a.get("note.md"); got != "v2 from B\n" {
		t.Errorf("note.md = %q, want B's version at the path", got)
	}
	if len(copies) != 1 || a.get(copies[0]) != "v2 typed on A during sync\n" {
		t.Errorf("A's edit should survive as one conflict copy, got %v", copies)
	}
}

// Editor scratch files and OS metadata stay local.
func TestDefaultIgnoresKeepEditorLitterLocal(t *testing.T) {
	m := newMachine(t, "m")
	for _, name := range []string{"note.md", ".note.md.swp", "note.md~", "4913", ".DS_Store", ".#note.md", ".tmp-123"} {
		m.put(t, name, "x")
	}
	sha, _, err := snapshotWorkingTree(m.s, "test")
	if err != nil {
		t.Fatal(err)
	}
	man, err := m.s.readManifest(sha)
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(man.Files); len(got) != 1 || got[0] != "note.md" {
		t.Errorf("synced files = %v, want only note.md", got)
	}
}
