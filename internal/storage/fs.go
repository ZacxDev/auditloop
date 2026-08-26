package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FS is a local-filesystem Store. Objects live under Root/<key>. PresignGet
// returns an app-relative proxy path (/artifacts/<key>) — the filesystem
// backend has no signed URLs, so screenshots are served via the app's authed
// artifact proxy instead of a public bucket.
type FS struct {
	Root      string
	ProxyBase string // e.g. "/artifacts" — used to build PresignGet paths
}

// NewFS creates a filesystem store rooted at root.
func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &FS{Root: root, ProxyBase: "/artifacts"}, nil
}

func (f *FS) Backend() string { return "fs" }

func (f *FS) path(key string) string {
	return filepath.Join(f.Root, filepath.FromSlash(cleanKey(key)))
}

func (f *FS) Put(_ context.Context, key, _ string, r io.Reader, _ int64) error {
	p := f.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	fh, err := os.Create(p)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = io.Copy(fh, r)
	return err
}

func (f *FS) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(f.path(key))
}

func (f *FS) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return f.ProxyBase + "/" + cleanKey(key), nil
}

func (f *FS) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	base := f.Root
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if prefix == "" || strings.HasPrefix(key, cleanKey(prefix)) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// cleanKey normalizes a storage key and refuses path traversal.
func cleanKey(key string) string {
	key = strings.TrimPrefix(key, "/")
	// Reject traversal defensively (keys are app-generated, but belt-and-braces).
	key = strings.ReplaceAll(key, "..", "")
	return key
}
