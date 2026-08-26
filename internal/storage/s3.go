package storage

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 is a MinIO/S3-API Store.
type S3 struct {
	client *minio.Client
	bucket string
}

// S3Config configures the S3 backend.
type S3Config struct {
	Endpoint     string // host:port, no scheme
	Bucket       string
	AccessKey    string
	SecretKey    string
	Region       string
	UseSSL       bool
	UsePathStyle bool
}

// NewS3 connects to the S3 endpoint and ensures the bucket exists.
func NewS3(ctx context.Context, c S3Config) (*S3, error) {
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: c.UseSSL,
		Region: c.Region,
	}
	if c.UsePathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	cl, err := minio.New(c.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("storage: s3 connect: %w", err)
	}
	s := &S3{client: cl, bucket: c.Bucket}
	exists, err := cl.BucketExists(ctx, c.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage: bucket check: %w", err)
	}
	if !exists {
		if err := cl.MakeBucket(ctx, c.Bucket, minio.MakeBucketOptions{Region: c.Region}); err != nil {
			return nil, fmt.Errorf("storage: make bucket: %w", err)
		}
	}
	return s, nil
}

func (s *S3) Backend() string { return "s3" }

func (s *S3) Put(ctx context.Context, key, contentType string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, cleanKey(key), r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, cleanKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// Force an error now if the object is missing (GetObject is lazy).
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, cleanKey(key), ttl, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: cleanKey(prefix), Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}
