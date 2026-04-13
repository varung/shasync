package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

type gcsRemote struct {
	client *storage.Client
	bucket string
	prefix string
}

func newGCSRemote(ctx context.Context, bucket, prefix string) (Remote, error) {
	var opts []option.ClientOption
	// STORAGE_EMULATOR_HOST is honored by the client library natively; we only
	// need to skip auth when an emulator is in use. We also force reads through
	// the JSON API because fake-gcs-server does not implement the XML range
	// reader path the default reader uses.
	if os.Getenv("STORAGE_EMULATOR_HOST") != "" {
		opts = append(opts, option.WithoutAuthentication(), storage.WithJSONReads())
	}
	c, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &gcsRemote{client: c, bucket: bucket, prefix: strings.TrimSuffix(prefix, "/")}, nil
}

func (g *gcsRemote) key(k string) string {
	if g.prefix == "" {
		return k
	}
	return g.prefix + "/" + k
}

func (g *gcsRemote) Exists(ctx context.Context, key string) (bool, error) {
	_, err := g.client.Bucket(g.bucket).Object(g.key(key)).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (g *gcsRemote) Upload(ctx context.Context, key string, r io.Reader) error {
	w := g.client.Bucket(g.bucket).Object(g.key(key)).NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (g *gcsRemote) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	return g.client.Bucket(g.bucket).Object(g.key(key)).NewReader(ctx)
}
