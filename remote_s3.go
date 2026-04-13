package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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
	if ep := os.Getenv("SHASYNC_S3_ENDPOINT"); ep != "" {
		optFns = append(optFns, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ep)
		})
	}
	if os.Getenv("SHASYNC_S3_FORCE_PATH_STYLE") == "1" {
		optFns = append(optFns, func(o *s3.Options) {
			o.UsePathStyle = true
		})
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
