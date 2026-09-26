// Package s3 implements storage.Store on S3-compatible object storage.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/tiim/photo-collect/internal/storage"
)

type Options struct {
	Endpoint  string // empty for AWS
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

type Store struct {
	client   *awss3.Client
	uploader *manager.Uploader
	bucket   string
}

func New(ctx context.Context, o Options) (*Store, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.Region)}
	if o.AccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, err
	}
	client := awss3.NewFromConfig(cfg, func(so *awss3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.PathStyle
	})
	return &Store{client: client, uploader: manager.NewUploader(client), bucket: o.Bucket}, nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, _ int64, contentType string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	in := &awss3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: r}
	if contentType != "" {
		in.ContentType = &contentType
	}
	// The uploader streams unseekable readers via multipart uploads.
	_, err := s.uploader.Upload(ctx, in)
	return err
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return out.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return err
}

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}

func (s *Store) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &s.bucket})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) {
			return fmt.Errorf("s3 bucket %q: %s", s.bucket, ae.ErrorCode())
		}
	}
	return err
}
