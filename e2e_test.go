package main

import (
	"context"
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
	must(t, cmdCommit([]string{"-m", "first"}))

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
	must(t, cmdCommit([]string{"-m", "edit"}))
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
	must(t, cmdCommit([]string{"-m", "A-first"}))
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
