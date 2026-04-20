package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Remote is an abstract blob store. Keys are slash-separated strings.
//
// List returns keys under the given prefix, relative to the remote root
// (the caller-supplied prefix is *not* stripped — e.g. List(ctx, "manifests/")
// returns keys like "manifests/<sha>"). Listing requires read-list permission
// on the bucket (s3:ListBucket / storage.objects.list); it is only used by
// discovery commands like `shasync log --remote`, never in the hot path.
type Remote interface {
	Exists(ctx context.Context, key string) (bool, error)
	Upload(ctx context.Context, key string, r io.Reader) error
	Download(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]string, error)
}

// Remote URL formats:
//   gs://<bucket>/<prefix...>
//   s3://<bucket>/<prefix...>
//
// Optional env-var escape hatches used by tests:
//   SHASYNC_S3_ENDPOINT      — custom S3 endpoint (e.g. http://localhost:9000)
//   SHASYNC_S3_FORCE_PATH_STYLE=1
//   STORAGE_EMULATOR_HOST  — standard GCS emulator env (e.g. http://localhost:8080)
func newRemote(ctx context.Context, url string) (Remote, error) {
	switch {
	case strings.HasPrefix(url, "gs://"):
		rest := strings.TrimPrefix(url, "gs://")
		bucket, prefix := splitBucketPrefix(rest)
		return newGCSRemote(ctx, bucket, prefix)
	case strings.HasPrefix(url, "s3://"):
		rest := strings.TrimPrefix(url, "s3://")
		bucket, prefix := splitBucketPrefix(rest)
		return newS3Remote(ctx, bucket, prefix)
	default:
		return nil, fmt.Errorf("unsupported remote url: %s (expected gs:// or s3://)", url)
	}
}

func splitBucketPrefix(s string) (string, string) {
	s = strings.Trim(s, "/")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func remoteBlobKey(sha string) string     { return "blobs/" + sha }
func remoteManifestKey(sha string) string { return "manifests/" + sha }

// remoteHeadKey is the shared mutable pointer at <prefix>/HEAD. It contains a
// single 64-hex-char SHA (the current tip manifest) plus a newline.
// Stored plaintext even when encryption is on: the SHA is already the public
// name of the manifest object, so the pointer leaks no additional information.
const remoteHeadKey = "HEAD"

// streamToFile writes rc to path via a temp file + rename, setting mode.
func streamToFile(rc io.Reader, path string, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// parallelFor runs fn on each item with N workers; returns the first error.
func parallelFor[T any](ctx context.Context, workers int, items []T, fn func(context.Context, T) error) error {
	if len(items) == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	ch := make(chan T)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				if err := fn(ctx, it); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
outer:
	for _, it := range items {
		select {
		case <-ctx.Done():
			break outer
		case ch <- it:
		}
	}
	close(ch)
	wg.Wait()
	return firstErr
}
