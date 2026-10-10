package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
	if err := syncOnce(context.Background(), m.s, r, testKey(t), m.c); err != nil && err != errLocalEditsDuringSync {
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
	if err := fetchAndCheckout(context.Background(), r, a.s, testKey(t), head); err != nil {
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

// --- Encryption is mandatory ----------------------------------------------

// snapshotBucket copies the remote's objects, to prove an operation changed nothing.
func (m *memRemote) snapshotBucket() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.objs {
		out[k] = string(v)
	}
	return out
}

// A machine that has lost its key — even one holding the whole history
// locally — must not push anything, let alone plaintext.
func TestKeylessMachineCannotPush(t *testing.T) {
	r, a, _ := twoMachines(t, map[string]string{"secret.md": "v1\n"})
	a.put(t, "secret.md", "a private edit\n")
	before := r.snapshotBucket()

	err := syncOnce(context.Background(), a.s, r, nil, a.c)
	if err == nil {
		t.Fatal("sync without a key succeeded")
	}
	if !strings.Contains(err.Error(), "no encryption key") {
		t.Errorf("unexpected error: %v", err)
	}
	if after := r.snapshotBucket(); len(after) != len(before) {
		t.Errorf("bucket changed: %d objects before, %d after", len(before), len(after))
	}
	for k, v := range r.snapshotBucket() {
		if strings.Contains(v, "a private edit") {
			t.Fatalf("plaintext reached the bucket at %s", k)
		}
	}
}

// Every blob and manifest that reaches the bucket is encrypted.
func TestEverythingUploadedIsEncrypted(t *testing.T) {
	r, a, b := twoMachines(t, map[string]string{"one.md": "first note\n", "two.md": "second note\n"})
	a.put(t, "three.md", "third note\n")
	a.sync(t, r)
	b.sync(t, r)
	var checked int
	for k, v := range r.snapshotBucket() {
		if !strings.HasPrefix(k, "blobs/") && !strings.HasPrefix(k, "manifests/") {
			continue // HEAD, salt and push-log hold hashes, not content
		}
		checked++
		if !isEncryptedObject([]byte(v)) {
			t.Errorf("%s is not encrypted", k)
		}
		for _, secret := range []string{"first note", "second note", "third note", ".md"} {
			if strings.Contains(v, secret) {
				t.Errorf("%s contains plaintext %q", k, secret)
			}
		}
	}
	if checked < 5 {
		t.Fatalf("only %d content objects checked", checked)
	}
}

// A plaintext object in the bucket (written by anything but shasync) is
// refused rather than trusted.
func TestPlaintextObjectInBucketIsRefused(t *testing.T) {
	r, _, b := twoMachines(t, map[string]string{"note.md": "v1\n"})
	plain := "planted plaintext\n"
	sha, _, err := hashBytesForTest(plain)
	if err != nil {
		t.Fatal(err)
	}
	r.objs["blobs/"+sha] = []byte(plain)
	err = downloadBlob(context.Background(), r, b.s, testKey(t), sha)
	if err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("plaintext blob accepted (err = %v)", err)
	}
}

// The commands themselves refuse to start without a key.
func TestCommandsRequireKey(t *testing.T) {
	t.Setenv("SHASYNC_KEY", "")
	m := newMachine(t, "m")
	if _, err := m.s.requireKey(); err == nil {
		t.Fatal("requireKey succeeded with no key")
	}
	m.put(t, "note.md", "private\n")
	r := &memRemote{objs: map[string][]byte{}}
	err := uploadFile(context.Background(), r, nil, filepath.Join(m.s.Root, "note.md"), "blobs/x")
	if err == nil || !strings.Contains(err.Error(), "no encryption key") {
		t.Fatalf("uploadFile with a nil key: err = %v", err)
	}
	if len(r.objs) != 0 {
		t.Fatal("uploadFile with a nil key wrote to the bucket")
	}
}

func hashBytesForTest(s string) (string, int64, error) {
	f, err := os.CreateTemp("", "shasync-hash-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(s); err != nil {
		return "", 0, err
	}
	f.Close()
	return hashFile(f.Name())
}

// --- Bucket privacy check -------------------------------------------------

type fakeAuditedRemote struct {
	memRemote
	report *bucketReport
}

func (f *fakeAuditedRemote) auditBucket(context.Context) *bucketReport { return f.report }

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	fn()
	wr.Close()
	os.Stdout = old
	out, _ := io.ReadAll(rd)
	return string(out)
}

func TestBucketCheckWarnsWhenPublic(t *testing.T) {
	r := &fakeAuditedRemote{report: &bucketReport{Bucket: "b",
		Public: []string{"the ACL grants READ to EVERYONE"},
		Risks:  []string{"these are the AWS account's ROOT access keys — use an IAM user limited to this bucket instead"}}}
	out := captureStdout(t, func() { checkBucketBeforeSync(context.Background(), r) })
	for _, want := range []string{"MAY BE PUBLIC", "READ to EVERYONE", "ROOT access keys"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestBucketCheckQuietWhenPrivate(t *testing.T) {
	r := &fakeAuditedRemote{report: &bucketReport{Bucket: "b", Facts: []string{"Block Public Access: all on"}}}
	out := captureStdout(t, func() { checkBucketBeforeSync(context.Background(), r) })
	if strings.TrimSpace(out) != "bucket b: private ✓" {
		t.Errorf("unexpected output: %q", out)
	}
}

// Identical files ingested concurrently (commit uses parallel workers) must
// not race on the shared object path.
func TestIngestIdenticalFilesConcurrently(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Root: dir}
	content := bytes.Repeat([]byte("same picture "), 5000)
	var paths []string
	for i := 0; i < 16; i++ {
		p := filepath.Join(dir, fmt.Sprintf("copy%d.png", i))
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	for round := 0; round < 20; round++ {
		os.RemoveAll(filepath.Join(dir, ".blobs"))
		var wg sync.WaitGroup
		errs := make(chan error, len(paths))
		for _, p := range paths {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				if _, _, err := s.ingestFile(p); err != nil {
					errs <- err
				}
			}(p)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

// A fresh, empty clone must fast-forward to the remote on its first watch
// cycle, not treat the empty local HEAD as a fork ("no common ancestor").
func TestWatchSyncFreshEmptyCloneFastForwards(t *testing.T) {
	r := &memRemote{objs: map[string][]byte{}}
	b := newMachine(t, "machineB")
	b.put(t, "a.txt", "alpha")
	b.put(t, "b.txt", "bravo")
	b.sync(t, r)
	remote, err := readRemoteHead(context.Background(), r)
	if err != nil || remote == "" {
		t.Fatalf("remote HEAD = %q, %v", remote, err)
	}

	a := newMachine(t, "machineA")
	for i := 0; i < 2; i++ { // every cycle must succeed, not just the first
		if err := syncOnce(context.Background(), a.s, r, testKey(t), a.c); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if head, _ := a.s.readHead(); head != remote {
		t.Fatalf("local HEAD = %q, want remote %q", head, remote)
	}
	if a.get("a.txt") != "alpha" || a.get("b.txt") != "bravo" {
		t.Fatalf("files not checked out: a=%q b=%q", a.get("a.txt"), a.get("b.txt"))
	}
}

// snapshotWithTimeout runs snapshotWorkingTree, failing the test if it hangs.
func snapshotWithTimeout(t *testing.T, s *Store) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := snapshotWorkingTree(s, "test")
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("snapshotWorkingTree deadlocked")
		return nil
	}
}

// Unreadable files make commit fail (as one always did) — and many of them
// (more than the worker count) must not deadlock it.
func TestCommitWithUnreadableFilesErrorsWithoutHanging(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read chmod-000 files")
	}
	for _, n := range []int{1, 8, 50} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			m := newMachine(t, "m")
			m.put(t, "ok.txt", "fine")
			for i := 0; i < n; i++ {
				p := filepath.Join(m.s.Root, fmt.Sprintf("locked%02d.txt", i))
				if err := os.WriteFile(p, []byte(fmt.Sprint(i)), 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(p, 0o644) })
			}
			err := snapshotWithTimeout(t, m.s)
			if err == nil || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("err = %v, want permission error", err)
			}
			if head, _ := m.s.readHead(); head != "" {
				t.Fatalf("HEAD advanced to %s despite errors", head)
			}
		})
	}
}

// A file that changes between ingestFile's hash and its copy into the store
// must still be stored under the SHA of the bytes actually stored; otherwise
// every other machine fails the SHA check downloading it, forever.
func TestIngestFileChangedBetweenHashAndCopy(t *testing.T) {
	dir := t.TempDir()
	if err := initStore(dir); err != nil {
		t.Fatal(err)
	}
	s := &Store{Root: dir}
	p := filepath.Join(dir, "note.md")
	if err := os.WriteFile(p, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	ingestAfterHashHook = func(string) {
		if err := os.WriteFile(p, []byte("after, and longer"), 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { ingestAfterHashHook = nil })

	sha, size, err := s.ingestFile(p)
	if err != nil {
		t.Fatal(err)
	}
	stored, storedSize, err := hashFile(s.objectPath(sha))
	if err != nil {
		t.Fatalf("no object under returned sha %s: %v", shortSHA(sha), err)
	}
	if stored != sha || storedSize != size {
		t.Fatalf("object %s holds bytes hashing to %s (size %d, returned %d)", shortSHA(sha), shortSHA(stored), storedSize, size)
	}
	if want, _, _ := hashBytesForTest("after, and longer"); sha != want {
		t.Fatalf("sha = %s, want hash of the stored (new) content", shortSHA(sha))
	}
	objs, _ := s.listLocalObjects()
	if len(objs) != 1 {
		t.Fatalf("objects = %v, want exactly one (no stray temp or mislabeled object)", objs)
	}
}

// The push-log cache describes what the current remote already holds; after
// `remote set` points the repo somewhere else it must be dropped, or push
// skips blobs and manifests the new bucket has never seen.
func TestRemoteSetClearsPushLogCacheWhenRemoteChanges(t *testing.T) {
	m := newMachine(t, "m")
	chdir(t, m.s.Root)
	writeCache := func() {
		t.Helper()
		if err := m.s.writePushLogCache(&pushLogCache{Cursor: "x", Known: map[string]struct{}{"abc": {}}}); err != nil {
			t.Fatal(err)
		}
	}
	cacheExists := func() bool {
		_, err := os.Stat(m.s.pushLogCachePath())
		return err == nil
	}
	set := func(args ...string) {
		t.Helper()
		if err := cmdRemote(context.Background(), append([]string{"set"}, args...)); err != nil {
			t.Fatal(err)
		}
	}

	writeCache()
	set("s3://old-bucket/notes")
	if cacheExists() {
		t.Fatal("cache kept after setting a remote for the first time")
	}

	writeCache()
	set("s3://old-bucket/notes")
	if !cacheExists() {
		t.Fatal("cache dropped although the remote did not change")
	}

	set("s3://new-bucket/notes")
	if cacheExists() {
		t.Fatal("cache kept after pointing at a new bucket")
	}

	writeCache()
	set("--endpoint", "https://other.example", "s3://new-bucket/notes")
	if cacheExists() {
		t.Fatal("cache kept after changing the endpoint")
	}
}
