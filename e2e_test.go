package main

import (
	"context"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// makeWorkTree creates a scratch working directory with some files.
func makeWorkTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "hello.txt"), "hello world\n")
	write(t, filepath.Join(dir, "sub", "a.txt"), "alpha\n")
	write(t, filepath.Join(dir, "sub", "b.bin"), "\x00\x01\x02\x03bin\n")
	return dir
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// chdir cd's into dir for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// --- Pure-local round-trip (no remote). ---

func TestCommitAndCheckoutRoundTrip(t *testing.T) {
	a := makeWorkTree(t)
	chdir(t, a)

	must(t, cmdInit())
	must(t, cmdCommit(context.Background(), []string{"-m", "first"}))

	s, err := findStore()
	if err != nil {
		t.Fatal(err)
	}
	head1, _ := s.readHead()
	if head1 == "" {
		t.Fatal("no HEAD after commit")
	}

	// Modify a file, commit again.
	write(t, filepath.Join(a, "hello.txt"), "changed\n")
	must(t, cmdCommit(context.Background(), []string{"-m", "edit"}))
	head2, _ := s.readHead()
	if head1 == head2 {
		t.Fatal("expected new HEAD after edit")
	}

	// Go back to head1 — file should be restored.
	must(t, cmdCheckout([]string{head1}))
	if got := read(t, filepath.Join(a, "hello.txt")); got != "hello world\n" {
		t.Fatalf("checkout did not restore hello.txt: got %q", got)
	}

	// Forward to head2 again.
	must(t, cmdCheckout([]string{head2}))
	if got := read(t, filepath.Join(a, "hello.txt")); got != "changed\n" {
		t.Fatalf("checkout did not restore head2: got %q", got)
	}
}

// --- S3 emulator round-trip. ---

func TestPushPullViaS3Emulator(t *testing.T) {
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewServer(faker.Server())
	t.Cleanup(srv.Close)

	// Pre-create bucket so first PUT doesn't fail.
	if err := backend.CreateBucket("shasync-test"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SHASYNC_S3_ENDPOINT", srv.URL)
	t.Setenv("SHASYNC_S3_FORCE_PATH_STYLE", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	runRemoteRoundTrip(t, "s3://shasync-test/project-x")
}

// --- GCS emulator round-trip. ---

func TestPushPullViaGCSEmulator(t *testing.T) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		Host:   "127.0.0.1",
		Port:   0,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "shasync-test"})

	// Point the GCS client library at the emulator.
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL())
	t.Logf("fake-gcs-server URL = %s", srv.URL())
	t.Cleanup(func() {
		objs, _ := srv.Backend().ListObjects("shasync-test", "", false)
		for _, o := range objs {
			t.Logf("stored: %s/%s", o.BucketName, o.Name)
		}
	})

	runRemoteRoundTrip(t, "gs://shasync-test/project-x")
}

// runRemoteRoundTrip: commit in dir A, push, pull in dir B, verify files.
func runRemoteRoundTrip(t *testing.T, remoteURL string) {
	t.Helper()
	ctx := context.Background()

	// --- Machine A: init, commit, push ---
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdCommit(context.Background(), []string{"-m", "A-first"}))
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPush(ctx, nil))

	sA, _ := findStore()
	head, _ := sA.readHead()
	if head == "" {
		t.Fatal("no HEAD on A")
	}

	// --- Machine B: fresh dir, init, set same remote, pull, checkout ---
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, []string{head}))
	must(t, cmdCheckout([]string{head}))

	for _, rel := range []string{"hello.txt", "sub/a.txt", "sub/b.bin"} {
		src := filepath.Join(a, rel)
		dst := filepath.Join(b, rel)
		if read(t, src) != read(t, dst) {
			t.Fatalf("file %s differs after push/pull/checkout", rel)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// --- Encryption round-trip on the S3 emulator. ---
//
// Verifies (1) the bucket objects are actually ciphertext (not plaintext), and
// (2) a second machine with the same key can pull + decrypt + checkout.

func TestEncryptedPushPullViaS3Emulator(t *testing.T) {
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewServer(faker.Server())
	t.Cleanup(srv.Close)
	if err := backend.CreateBucket("shasync-test"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SHASYNC_S3_ENDPOINT", srv.URL)
	t.Setenv("SHASYNC_S3_FORCE_PATH_STYLE", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	ctx := context.Background()
	remoteURL := "s3://shasync-test/encrypted-project"
	const marker = "hello world\n" // appears verbatim in makeWorkTree's hello.txt

	// --- Machine A ---
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdKey(ctx, []string{"gen"}))
	must(t, cmdCommit(context.Background(), []string{"-m", "encrypted"}))
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPush(ctx, nil))

	sA, _ := findStore()
	head, _ := sA.readHead()
	keyA, err := os.ReadFile(sA.keyPath())
	if err != nil {
		t.Fatal(err)
	}

	// Assert stored objects don't contain plaintext.
	objs, err := backend.ListBucket("shasync-test", nil, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs.Contents) == 0 {
		t.Fatal("expected uploaded objects, got none")
	}
	for _, o := range objs.Contents {
		got, err := backend.GetObject("shasync-test", o.Key, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(got.Contents)
		got.Contents.Close()
		if err != nil {
			t.Fatal(err)
		}
		if o.Key == "encrypted-project/salt" {
			continue // salt is stored plaintext by design
		}
		if o.Key == "encrypted-project/HEAD" {
			// HEAD is plaintext by design (a 64-hex SHA that's already the
			// public name of a manifest object — adds no information over
			// what a listing already exposes). Must not leak file content
			// though.
			if bytesContains(body, []byte(marker)) {
				t.Fatalf("object %s leaks plaintext marker %q", o.Key, marker)
			}
			continue
		}
		if len(body) < cryptMagicLen || string(body[:cryptMagicLen]) != cryptMagicV2 {
			t.Fatalf("object %s is not shasync-v2-encrypted: first bytes = %q", o.Key, body[:min(16, len(body))])
		}
		if bytesContains(body, []byte(marker)) {
			t.Fatalf("object %s leaks plaintext marker %q", o.Key, marker)
		}
	}

	// --- Machine B: same key, pull succeeds ---
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	sB, _ := findStore()
	if err := os.WriteFile(sB.keyPath(), keyA, 0o600); err != nil {
		t.Fatal(err)
	}
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, []string{head}))
	must(t, cmdCheckout([]string{head}))
	if read(t, filepath.Join(b, "hello.txt")) != marker {
		t.Fatalf("machine B did not recover plaintext")
	}

	// --- Machine C: wrong key, pull must fail ---
	c := t.TempDir()
	chdir(t, c)
	must(t, cmdInit())
	sC, _ := findStore()
	wrong, _ := generateKey()
	if err := os.WriteFile(sC.keyPath(), []byte(hex.EncodeToString(wrong)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	must(t, cmdRemote([]string{"set", remoteURL}))
	if err := cmdPull(ctx, []string{head}); err == nil {
		t.Fatal("expected pull with wrong key to fail")
	}
}

// --- Passphrase-derived key, end-to-end across two machines. ---

func TestPassphraseRoundTripViaS3Emulator(t *testing.T) {
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewServer(faker.Server())
	t.Cleanup(srv.Close)
	if err := backend.CreateBucket("shasync-test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHASYNC_S3_ENDPOINT", srv.URL)
	t.Setenv("SHASYNC_S3_FORCE_PATH_STYLE", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SHASYNC_PASSPHRASE", "correct horse battery staple")

	ctx := context.Background()
	remoteURL := "s3://shasync-test/passphrase-project"

	// --- Machine A: init, remote, key from passphrase, commit, push ---
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdKey(ctx, []string{"set-passphrase"}))
	must(t, cmdCommit(context.Background(), []string{"-m", "passphrase commit"}))
	must(t, cmdPush(ctx, nil))

	sA, _ := findStore()
	head, _ := sA.readHead()

	// --- Machine B: same passphrase, no copied key file ---
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdKey(ctx, []string{"set-passphrase"}))
	must(t, cmdPull(ctx, []string{head}))
	must(t, cmdCheckout([]string{head}))

	for _, rel := range []string{"hello.txt", "sub/a.txt", "sub/b.bin"} {
		if read(t, filepath.Join(a, rel)) != read(t, filepath.Join(b, rel)) {
			t.Fatalf("file %s differs after passphrase roundtrip", rel)
		}
	}

	// --- Machine C: wrong passphrase → pull must fail ---
	c := t.TempDir()
	chdir(t, c)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	t.Setenv("SHASYNC_PASSPHRASE", "wrong passphrase")
	must(t, cmdKey(ctx, []string{"set-passphrase"}))
	if err := cmdPull(ctx, []string{head}); err == nil {
		t.Fatal("expected pull with wrong passphrase to fail")
	}
}

// --- Chunked encryption: unit test for multi-chunk sizes. ---

func TestChunkedEncryptionRoundTripAllSizes(t *testing.T) {
	key, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{
		0, 1, 16, 1024,
		cryptChunkSize - 1,
		cryptChunkSize,
		cryptChunkSize + 1,
		3 * cryptChunkSize,
		3*cryptChunkSize + 7,
	} {
		pt := make([]byte, size)
		for i := range pt {
			pt[i] = byte(i * 131)
		}
		ct, err := encryptBytes(key, pt)
		if err != nil {
			t.Fatalf("size=%d: encrypt: %v", size, err)
		}
		if len(ct) >= cryptMagicLen && string(ct[:cryptMagicLen]) != cryptMagicV2 {
			t.Fatalf("size=%d: wrong magic on ciphertext", size)
		}
		got, err := decryptBytes(key, ct)
		if err != nil {
			t.Fatalf("size=%d: decrypt: %v", size, err)
		}
		if !bytesEqual(got, pt) {
			t.Fatalf("size=%d: roundtrip differs", size)
		}
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// --- Linear-history workflow tests --------------------------------------
//
// These exercise the full single-user "pull → commit → push" story across
// two machines sharing an S3 bucket: remote HEAD, pull-no-args, commit
// pre-flight, log --remote, and the divergent-push auto-merge with
// conflict-copy files.

func newS3Fake(t *testing.T, bucket string) string {
	t.Helper()
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewServer(faker.Server())
	t.Cleanup(srv.Close)
	if err := backend.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHASYNC_S3_ENDPOINT", srv.URL)
	t.Setenv("SHASYNC_S3_FORCE_PATH_STYLE", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	return srv.URL
}

// TestPullNoArgsFetchesRemoteHead: clean-clone scenario — pull with no SHA
// reads remote HEAD, fetches everything, and leaves the working tree synced.
func TestPullNoArgsFetchesRemoteHead(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/sync-project"

	// A: init + commit + push.
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdCommit(ctx, []string{"-m", "A-first"}))
	must(t, cmdPush(ctx, nil))
	sA, _ := findStore()
	headA, _ := sA.readHead()

	// B: fresh clone; pull with no args should materialize working tree.
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, nil))
	sB, _ := findStore()
	headB, _ := sB.readHead()
	if headB != headA {
		t.Fatalf("local HEAD after pull = %s, want %s", headB, headA)
	}
	for _, rel := range []string{"hello.txt", "sub/a.txt", "sub/b.bin"} {
		if read(t, filepath.Join(a, rel)) != read(t, filepath.Join(b, rel)) {
			t.Fatalf("file %s differs after clone-pull", rel)
		}
	}

	// Idempotency: second pull is a no-op.
	must(t, cmdPull(ctx, nil))
}

// TestLogRemoteWalksChain: push a few commits, then verify log --remote
// walks the chain in reverse-chronological order.
func TestLogRemoteWalksChain(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/log-project"

	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdCommit(ctx, []string{"-m", "c1"}))
	must(t, cmdPush(ctx, nil))
	write(t, filepath.Join(a, "hello.txt"), "v2\n")
	must(t, cmdCommit(ctx, []string{"-m", "c2"}))
	must(t, cmdPush(ctx, nil))
	write(t, filepath.Join(a, "hello.txt"), "v3\n")
	must(t, cmdCommit(ctx, []string{"-m", "c3"}))
	must(t, cmdPush(ctx, nil))

	// Fresh clone with no HEAD: log --remote should reach all three.
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	if err := cmdLog(ctx, []string{"--remote", "--summary"}); err != nil {
		t.Fatalf("log --remote: %v", err)
	}

	// After the call, local store should hold all three manifests.
	sB, _ := findStore()
	manCount, _ := countStoreDir(sB.manifestsPath())
	if manCount != 3 {
		t.Fatalf("expected 3 manifests cached after log --remote, got %d", manCount)
	}
}

// TestDivergentPushRefusesPullMerges: the divergent flow under the new
// design. Push refuses on divergence and tells the user to pull first;
// pull detects the fork, builds a merge manifest with conflict-copy files
// tagged with the local client's ID, and checks it out; a second push is
// then a plain fast-forward. A third machine pulls and sees the same state.
func TestDivergentPushRefusesPullMerges(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/merge-project"

	// A: establish base.
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdCommit(ctx, []string{"-m", "base"}))
	must(t, cmdPush(ctx, nil))

	// B: clone base.
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, nil))
	sB, _ := findStore()
	cfgB, _ := sB.readConfig()
	clientB := cfgB.ClientID
	if clientB == "" {
		t.Fatal("expected B to have a ClientID after init")
	}

	// A: change hello.txt, add a new file, commit + push.
	chdir(t, a)
	write(t, filepath.Join(a, "hello.txt"), "A-version\n")
	write(t, filepath.Join(a, "a-only.txt"), "A made this\n")
	must(t, cmdCommit(ctx, []string{"-m", "A-change"}))
	must(t, cmdPush(ctx, nil))

	// B: independently change hello.txt to a different content, change
	// sub/a.txt, commit offline. Now histories have diverged.
	chdir(t, b)
	write(t, filepath.Join(b, "hello.txt"), "B-version\n")
	write(t, filepath.Join(b, "sub", "a.txt"), "B-edited\n")
	must(t, cmdCommit(ctx, []string{"-m", "B-change"}))

	// B's first push must REFUSE — divergence is now resolved in pull, not push.
	if err := cmdPush(ctx, nil); err == nil {
		t.Fatal("expected push to refuse on divergence under the new design")
	}

	// B pulls: should auto-merge, populate conflict copy, update working tree.
	if err := cmdPull(ctx, nil); err != nil {
		t.Fatalf("pull (merge) failed: %v", err)
	}

	// hello.txt  → remote kept at the original path on conflict.
	// a-only.txt → remote-only addition, preserved.
	// sub/a.txt  → B-only change, preserved.
	if got := read(t, filepath.Join(b, "hello.txt")); got != "A-version\n" {
		t.Fatalf("hello.txt after pull-merge = %q, want A-version", got)
	}
	if got := read(t, filepath.Join(b, "a-only.txt")); got != "A made this\n" {
		t.Fatalf("a-only.txt after pull-merge = %q, want A made this", got)
	}
	if got := read(t, filepath.Join(b, "sub", "a.txt")); got != "B-edited\n" {
		t.Fatalf("sub/a.txt after pull-merge = %q, want B-edited", got)
	}

	// Conflict copy must be tagged with B's ClientID (not hostname), and hold
	// B's version of hello.txt.
	conflictFile := findConflictCopy(t, b, "hello", clientB)
	if conflictFile == "" {
		listDir(t, b)
		t.Fatalf("no conflict-copy file tagged with client %q found in %s", clientB, b)
	}
	if got := read(t, filepath.Join(b, conflictFile)); got != "B-version\n" {
		t.Fatalf("conflict copy content = %q, want B-version", got)
	}

	// B now pushes: this should be a plain fast-forward over A's tip.
	if err := cmdPush(ctx, nil); err != nil {
		t.Fatalf("post-merge push failed: %v", err)
	}

	// A third machine pulls and sees the merged state.
	c := t.TempDir()
	chdir(t, c)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, nil))
	if got := read(t, filepath.Join(c, "hello.txt")); got != "A-version\n" {
		t.Fatalf("C sees hello.txt = %q, want A-version", got)
	}
	if got := read(t, filepath.Join(c, conflictFile)); got != "B-version\n" {
		t.Fatalf("C sees conflict-copy = %q, want B-version", got)
	}

	// A pulls too and sees the same merged state — including B's conflict copy.
	chdir(t, a)
	must(t, cmdPull(ctx, nil))
	if got := read(t, filepath.Join(a, "hello.txt")); got != "A-version\n" {
		t.Fatalf("A sees hello.txt = %q, want A-version (own edit preserved)", got)
	}
	if got := read(t, filepath.Join(a, conflictFile)); got != "B-version\n" {
		t.Fatalf("A sees conflict-copy = %q, want B-version", got)
	}
}

// findConflictCopy returns the basename of the first file in dir matching
// "<stem> (modified on <date> by <clientID>)<any ext>", or "" if none.
func findConflictCopy(t *testing.T, dir, stem, clientID string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefix := stem + " (modified on "
	suffix := " by " + clientID + ")"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		// Strip extension (what's after the last dot) and check the
		// "by <clientID>)" tag is at the end of the bare filename.
		stemPart := n
		if dot := strings.LastIndex(n, "."); dot > len(prefix) {
			stemPart = n[:dot]
		}
		if strings.HasSuffix(stemPart, suffix) {
			return n
		}
	}
	return ""
}

func listDir(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Logf("  %s", e.Name())
	}
}

// TestFastForwardPush: typical single-user flow — commit on A, push, pull on
// B, commit on B, push. B's push is a fast-forward that extends remote.
func TestFastForwardPush(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/ff-project"

	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdCommit(ctx, []string{"-m", "c1"}))
	must(t, cmdPush(ctx, nil))

	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, nil))

	// B adds a new file on top of the same base; commit + push should FF.
	write(t, filepath.Join(b, "new.txt"), "made on B\n")
	must(t, cmdCommit(ctx, []string{"-m", "c2-on-B"}))
	must(t, cmdPush(ctx, nil))

	// A pulls and sees new.txt.
	chdir(t, a)
	must(t, cmdPull(ctx, nil))
	if got := read(t, filepath.Join(a, "new.txt")); got != "made on B\n" {
		t.Fatalf("A sees new.txt = %q, want 'made on B'", got)
	}
}

// TestPushForceOverwritesRemote: --force bypasses the topology check. Useful
// for recovery (wipe remote, publish a known-good local tip) — we verify it
// successfully overwrites even when local is strictly behind.
func TestPushForceOverwritesRemote(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/force-project"

	// A advances the remote two commits ahead.
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdCommit(ctx, []string{"-m", "c1"}))
	must(t, cmdPush(ctx, nil))
	write(t, filepath.Join(a, "hello.txt"), "c2\n")
	must(t, cmdCommit(ctx, []string{"-m", "c2"}))
	must(t, cmdPush(ctx, nil))
	sA, _ := findStore()
	headAdvanced, _ := sA.readHead()

	// B has only the c1 snapshot (pulled before c2). Its HEAD is strictly
	// behind remote. A normal push would refuse with "pull first".
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	// Seed B at c1 by pulling then reverting the remote by force from B.
	// Actually: easier is to have B commit from scratch with no parent,
	// yielding a chain with NO common ancestor with remote — still a
	// scenario --force should resolve by overwriting.
	write(t, filepath.Join(b, "hello.txt"), "from-B-standalone\n")
	must(t, cmdCommit(ctx, []string{"-m", "B-standalone"}))
	sB, _ := findStore()
	headB, _ := sB.readHead()
	if headB == headAdvanced {
		t.Fatal("test setup invariant: expected B's HEAD to differ from A's")
	}

	// Without --force, push should refuse: no common ancestor → not a FF.
	if err := cmdPush(ctx, nil); err == nil {
		t.Fatal("expected unforced push to refuse unrelated histories")
	}

	// With --force, push overwrites remote HEAD regardless.
	must(t, cmdPush(ctx, []string{"--force"}))

	r, err := newRemote(ctx, remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	rh, err := readRemoteHead(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if rh != headB {
		t.Fatalf("remote HEAD after --force = %s, want %s", rh, headB)
	}
}

// TestPullAndLogOnEmptyRemote: before anyone has pushed, `pull` with no
// args should error out with a clear message (not silently succeed), and
// `log --remote` should print "(remote has no HEAD)" rather than error.
func TestPullAndLogOnEmptyRemote(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/empty-project"

	dir := t.TempDir()
	chdir(t, dir)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))

	// pull with no args: remote HEAD is absent → expected error.
	if err := cmdPull(ctx, nil); err == nil {
		t.Fatal("expected pull on empty remote to error")
	}

	// log --remote on empty remote: should NOT error; prints a friendly note.
	if err := cmdLog(ctx, []string{"--remote"}); err != nil {
		t.Fatalf("log --remote on empty remote errored: %v", err)
	}
}

// TestPushAutoCommitsDirtyTree: exercise the core premise of the
// implicit-commit model. User edits files and runs `push` directly (no
// explicit commit). Push must snapshot the tree into a new manifest,
// upload it, and advance remote HEAD. No "nothing to push" error, no
// silently-ignored edits.
func TestPushAutoCommitsDirtyTree(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/autocommit-project"

	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))

	// No explicit commit — straight to push. The initial files from
	// makeWorkTree are uncommitted; push must snapshot them.
	must(t, cmdPush(ctx, nil))
	sA, _ := findStore()
	head1, _ := sA.readHead()
	if head1 == "" {
		t.Fatal("push didn't produce a local commit")
	}

	// Confirm it materialized on the remote.
	r, err := newRemote(ctx, remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	rh1, err := readRemoteHead(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if rh1 != head1 {
		t.Fatalf("remote HEAD = %s, want %s", rh1, head1)
	}

	// Now edit without committing, push again — second implicit commit.
	write(t, filepath.Join(a, "hello.txt"), "second version\n")
	must(t, cmdPush(ctx, nil))
	head2, _ := sA.readHead()
	if head2 == head1 {
		t.Fatal("second push should have advanced HEAD via implicit commit")
	}
	rh2, err := readRemoteHead(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if rh2 != head2 {
		t.Fatalf("remote HEAD = %s, want %s", rh2, head2)
	}

	// Clean-tree push must be a no-op at the HEAD level: no new implicit
	// commit, no advance.
	must(t, cmdPush(ctx, nil))
	head3, _ := sA.readHead()
	if head3 != head2 {
		t.Fatalf("clean-tree push created a ghost commit: HEAD went %s → %s", head2, head3)
	}
}

// TestTwoFoldersImplicitCommitFlow is the headline end-to-end: two folders
// on the same remote, driven only by `pull` / edit / `push` (and `init` /
// `remote set` for setup). No explicit `commit` anywhere.
//
// Scenarios covered in sequence:
//  1. Normal sync: A publishes a change, B picks it up, B publishes a
//     change, A picks it up. Each machine sees the other's edits.
//  2. Offline divergence with a true conflict: both machines edit the same
//     file while offline. A pushes first. B's push refuses. B pulls →
//     auto-merge creates a conflict-copy tagged with B's client_id. B
//     pushes the merge. A pulls and sees the merged state, including the
//     conflict-copy file.
//
// The interesting invariants:
//   - The conflict-copy file is tagged with the client_id of the machine
//     whose version got stashed (B's), not A's.
//   - A third fresh clone pulling from scratch sees identical state.
//   - No work is lost — both sides' blobs survive as a sibling pair.
func TestTwoFoldersImplicitCommitFlow(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/two-folder-project"

	// --- Setup: A initializes a repo with some files and publishes it. ---
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPush(ctx, nil)) // implicit commit, then upload

	sA, _ := findStore()
	cfgA, _ := sA.readConfig()
	clientA := cfgA.ClientID

	// --- B: fresh clone via pull. ---
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, nil))

	sB, _ := findStore()
	cfgB, _ := sB.readConfig()
	clientB := cfgB.ClientID
	if clientB == clientA {
		t.Fatalf("two clones must have distinct client_ids (got %s on both)", clientA)
	}
	// Sanity: B has A's files.
	if read(t, filepath.Join(b, "hello.txt")) != "hello world\n" {
		t.Fatal("B didn't receive A's hello.txt after pull")
	}

	// === Scenario 1: normal sync, no conflicts ===================== //

	// A edits, pushes.
	chdir(t, a)
	write(t, filepath.Join(a, "hello.txt"), "A edit 1\n")
	must(t, cmdPush(ctx, nil))

	// B pulls, sees A's edit.
	chdir(t, b)
	must(t, cmdPull(ctx, nil))
	if got := read(t, filepath.Join(b, "hello.txt")); got != "A edit 1\n" {
		t.Fatalf("B sees hello.txt = %q, want %q after A's edit", got, "A edit 1\n")
	}

	// B edits a different file, pushes — fast-forward.
	write(t, filepath.Join(b, "sub", "a.txt"), "B edit 1\n")
	must(t, cmdPush(ctx, nil))

	// A pulls, sees B's edit. (A still has its own hello.txt untouched.)
	chdir(t, a)
	must(t, cmdPull(ctx, nil))
	if got := read(t, filepath.Join(a, "sub", "a.txt")); got != "B edit 1\n" {
		t.Fatalf("A sees sub/a.txt = %q, want B edit 1", got)
	}
	if got := read(t, filepath.Join(a, "hello.txt")); got != "A edit 1\n" {
		t.Fatalf("A's own hello.txt got clobbered: = %q", got)
	}

	// === Scenario 2: offline divergence with a real conflict ======= //

	// Both A and B edit hello.txt while "offline" (neither pulls).
	// A also adds a brand-new file (no conflict) and pushes first.
	chdir(t, a)
	write(t, filepath.Join(a, "hello.txt"), "A offline edit\n")
	write(t, filepath.Join(a, "a-only.md"), "made on A\n")
	must(t, cmdPush(ctx, nil))

	// B, having not pulled, edits the same hello.txt differently and
	// separately touches a non-conflicting file. Then tries to push.
	chdir(t, b)
	write(t, filepath.Join(b, "hello.txt"), "B offline edit\n")
	write(t, filepath.Join(b, "sub", "a.txt"), "B edit 2\n")

	// B's push must refuse — not a fast-forward.
	if err := cmdPush(ctx, nil); err == nil {
		t.Fatal("expected B's push to refuse on divergence; new model pushes no auto-merge")
	}

	// B pulls to resolve. Pull must:
	//   - implicit-commit B's uncommitted edits
	//   - detect divergence
	//   - auto-merge: hello.txt keeps A's version at the original path,
	//                 B's version is stashed as a sibling copy with B's client_id
	//   - sub/a.txt: only B changed, kept as B's version
	//   - a-only.md: only A added, kept at the original path
	//   - check out merge into B's working tree
	if err := cmdPull(ctx, nil); err != nil {
		t.Fatalf("B pull (merge) failed: %v", err)
	}

	// Assertions on B's working tree after merge.
	if got := read(t, filepath.Join(b, "hello.txt")); got != "A offline edit\n" {
		t.Fatalf("B hello.txt after merge = %q, want A's version", got)
	}
	if got := read(t, filepath.Join(b, "sub", "a.txt")); got != "B edit 2\n" {
		t.Fatalf("B sub/a.txt after merge = %q, want B's non-conflicting edit preserved", got)
	}
	if got := read(t, filepath.Join(b, "a-only.md")); got != "made on A\n" {
		t.Fatalf("B a-only.md after merge = %q, want A's new file", got)
	}

	// Conflict copy must be tagged with B's client_id and carry B's content.
	conflictFile := findConflictCopy(t, b, "hello", clientB)
	if conflictFile == "" {
		listDir(t, b)
		t.Fatalf("no conflict-copy file tagged with client %q found in %s", clientB, b)
	}
	if got := read(t, filepath.Join(b, conflictFile)); got != "B offline edit\n" {
		t.Fatalf("conflict copy content = %q, want B offline edit", got)
	}

	// B pushes the merge — plain fast-forward now.
	must(t, cmdPush(ctx, nil))

	// A pulls, sees the merged state including the conflict-copy file.
	chdir(t, a)
	must(t, cmdPull(ctx, nil))
	if got := read(t, filepath.Join(a, "hello.txt")); got != "A offline edit\n" {
		t.Fatalf("A hello.txt after pull = %q, want its own offline edit (kept at path on conflict)", got)
	}
	if got := read(t, filepath.Join(a, conflictFile)); got != "B offline edit\n" {
		t.Fatalf("A sees conflict copy = %q, want B offline edit", got)
	}
	if got := read(t, filepath.Join(a, "sub", "a.txt")); got != "B edit 2\n" {
		t.Fatalf("A sub/a.txt after pull = %q, want B edit 2", got)
	}

	// Third fresh clone sees identical state.
	c := t.TempDir()
	chdir(t, c)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, nil))
	if got := read(t, filepath.Join(c, "hello.txt")); got != "A offline edit\n" {
		t.Fatalf("C hello.txt = %q, want A offline edit", got)
	}
	if got := read(t, filepath.Join(c, conflictFile)); got != "B offline edit\n" {
		t.Fatalf("C conflict-copy = %q, want B offline edit", got)
	}
	if got := read(t, filepath.Join(c, "a-only.md")); got != "made on A\n" {
		t.Fatalf("C a-only.md = %q, want A's file", got)
	}
}

// TestClientIDStableAcrossRuns: init stamps a client_id; subsequent
// findStore + readConfig calls read back the same value, and two fresh
// inits in distinct directories produce distinct IDs.
func TestClientIDStableAcrossRuns(t *testing.T) {
	dir1 := t.TempDir()
	chdir(t, dir1)
	must(t, cmdInit())
	s1, _ := findStore()
	cfg1, _ := s1.readConfig()
	id1 := cfg1.ClientID
	if id1 == "" {
		t.Fatal("init didn't generate a client_id")
	}
	// Second findStore on the same dir must yield the same id.
	s1b, _ := findStore()
	cfg1b, _ := s1b.readConfig()
	if cfg1b.ClientID != id1 {
		t.Fatalf("client_id changed: %q → %q", id1, cfg1b.ClientID)
	}

	// A second init in a different directory yields a different id.
	dir2 := t.TempDir()
	chdir(t, dir2)
	must(t, cmdInit())
	s2, _ := findStore()
	cfg2, _ := s2.readConfig()
	if cfg2.ClientID == "" {
		t.Fatal("second init didn't generate a client_id")
	}
	if cfg2.ClientID == id1 {
		t.Fatalf("two fresh inits produced the same client_id %q — want distinct", id1)
	}
}
