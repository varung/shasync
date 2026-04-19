package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type s3Remote struct {
	client *s3.Client
	bucket string
	prefix string
}

func newS3Remote(ctx context.Context, bucket, prefix string) (Remote, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	var optFns []func(*s3.Options)
	customEndpoint := os.Getenv("SHASYNC_S3_ENDPOINT")
	if customEndpoint != "" {
		optFns = append(optFns, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(customEndpoint)
		})
	}
	if os.Getenv("SHASYNC_S3_FORCE_PATH_STYLE") == "1" {
		optFns = append(optFns, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}
	// Auto-detect the bucket's region so a misconfigured default region
	// doesn't turn every HeadObject into a 301. Skip when a custom endpoint
	// is in use (MinIO/Ceph/etc. don't speak the AWS bucket-region protocol).
	if customEndpoint == "" {
		probe := s3.NewFromConfig(cfg, optFns...)
		region, err := manager.GetBucketRegion(ctx, probe, bucket)
		if err != nil {
			return nil, fmt.Errorf("detect region for bucket %q: %w", bucket, err)
		}
		if region != "" && region != cfg.Region {
			cfg.Region = region
		}
	}
	c := s3.NewFromConfig(cfg, optFns...)
	return &s3Remote{client: c, bucket: bucket, prefix: strings.TrimSuffix(prefix, "/")}, nil
}

func (s *s3Remote) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

func (s *s3Remote) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err == nil {
		return true, nil
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return false, nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return false, nil
		}
	}
	return false, err
}

func (s *s3Remote) Upload(ctx context.Context, key string, r io.Reader) error {
	// Buffer to a seekable so the S3 client can compute content length / retry.
	// For simplicity we read into memory; good enough for typical blobs.
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
		Body:   strings.NewReader(string(body)),
	})
	return err
}

func (s *s3Remote) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}
