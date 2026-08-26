// Package storage abstracts artifact object storage. Two backends implement the
// same interface: S3 (MinIO/S3 API via minio-go — chosen over aws-sdk-go-v2 for
// its far smaller surface and one-call presigning, which is all we need) and a
// local filesystem backend used for hermetic tests and zero-dependency dev.
//
// Key scheme (content-addressable where sensible):
//
//	{target_slug}/{run_id}/{page_slug}/{viewport}.png
//	{target_slug}/{run_id}/{page_slug}/axe.json
//	{target_slug}/{run_id}/{page_slug}/network.json
//	{target_slug}/{run_id}/report.json
package storage

import (
	"context"
	"io"
	"time"
)

// Store is the artifact storage interface.
type Store interface {
	// Put uploads data under key with the given content type.
	Put(ctx context.Context, key, contentType string, r io.Reader, size int64) error
	// Get returns a reader for the object at key.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// PresignGet returns a time-limited GET URL for key. The filesystem backend
	// returns a path served by the app's authed proxy instead.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// List returns all keys under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// Backend reports the backend name ("s3" | "fs").
	Backend() string
}
