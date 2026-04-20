package main

import (
	"context"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestCommitRefusesWhenRemoteAhead: machine B commits without pulling first;
// the pre-flight check must refuse unless --offline is passed.
func TestCommitRefusesWhenRemoteAhead(t *testing.T) {
	newS3Fake(t, "shasync-test")
	ctx := context.Background()
	remoteURL := "s3://shasync-test/linear-project"

	// A: push an initial commit.
	a := makeWorkTree(t)
	chdir(t, a)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdCommit(ctx, []string{"-m", "A-first"}))
	must(t, cmdPush(ctx, nil))

	// B: clone by pull, then A pushes a *newer* commit while B is offline.
	b := t.TempDir()
	chdir(t, b)
	must(t, cmdInit())
	must(t, cmdRemote([]string{"set", remoteURL}))
	must(t, cmdPull(ctx, nil))

	// A moves forward.
	chdir(t, a)
	write(t, filepath.Join(a, "hello.txt"), "A-edit\n")
	must(t, cmdCommit(ctx, []string{"-m", "A-second"}))
	must(t, cmdPush(ctx, nil))

	// B tries to commit without pulling: must fail.
	chdir(t, b)
	write(t, filepath.Join(b, "sub", "a.txt"), "B-edit\n")
	if err := cmdCommit(ctx, []string{"-m", "B-oblivious"}); err == nil {
		t.Fatal("expected commit to refuse when remote is ahead")
	}

	// --offline lets it through.
	must(t, cmdCommit(ctx, []string{"-m", "B-offline", "--offline"}))
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

// TestDivergentPushAutoMerges: both machines commit independently from the
// same base. Push on the second machine must detect the fork, pull the
// remote tip, create a merge commit whose parent is remote HEAD, and write
// a conflict-copy for the single file both sides changed differently.
func TestDivergentPushAutoMerges(t *testing.T) {
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

	// A: change hello.txt to "A-version", push.
	chdir(t, a)
	write(t, filepath.Join(a, "hello.txt"), "A-version\n")
	// A also adds a new file only on A's side.
	write(t, filepath.Join(a, "a-only.txt"), "A made this\n")
	must(t, cmdCommit(ctx, []string{"-m", "A-change"}))
	must(t, cmdPush(ctx, nil))

	// B: change hello.txt to "B-version" and sub/a.txt (B-only), commit offline, push → should auto-merge.
	chdir(t, b)
	write(t, filepath.Join(b, "hello.txt"), "B-version\n")
	write(t, filepath.Join(b, "sub", "a.txt"), "B-edited\n")
	must(t, cmdCommit(ctx, []string{"--offline", "-m", "B-change"}))
	if err := cmdPush(ctx, nil); err != nil {
		t.Fatalf("auto-merge push failed: %v", err)
	}

	// After merge:
	//   hello.txt  → conflict. Kept remote ("A-version"). B's version saved
	//                as "hello (conflict from <host> <stamp>).txt".
	//   a-only.txt → remote-only addition, preserved as-is.
	//   sub/a.txt  → B-only change, preserved ("B-edited").
	if got := read(t, filepath.Join(b, "hello.txt")); got != "A-version\n" {
		t.Fatalf("hello.txt after merge = %q, want A-version (remote keeps path on conflict)", got)
	}
	if got := read(t, filepath.Join(b, "a-only.txt")); got != "A made this\n" {
		t.Fatalf("a-only.txt after merge = %q, want A made this", got)
	}
	if got := read(t, filepath.Join(b, "sub", "a.txt")); got != "B-edited\n" {
		t.Fatalf("sub/a.txt after merge = %q, want B-edited", got)
	}

	// Conflict copy must exist and contain B's version.
	entries, err := os.ReadDir(b)
	if err != nil {
		t.Fatal(err)
	}
	var conflictFile string
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > len("hello (conflict from ") &&
			e.Name()[:len("hello (conflict from ")] == "hello (conflict from " {
			conflictFile = e.Name()
			break
		}
	}
	if conflictFile == "" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no conflict-copy file found in %v", names)
	}
	if got := read(t, filepath.Join(b, conflictFile)); got != "B-version\n" {
		t.Fatalf("conflict copy content = %q, want B-version", got)
	}

	// Third machine pulls and sees the merged state.
	newS3Fake := t // keep tmp server alive for the duration
	_ = newS3Fake
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
