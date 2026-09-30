// Package storage defines the object storage abstraction used for images.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrNotFound is returned by Get when the key does not exist.
var ErrNotFound = errors.New("storage: object not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key      string
	Modified time.Time
	Size     int64
}

// Keys returns the keys of infos.
func Keys(infos []ObjectInfo) []string {
	keys := make([]string, len(infos))
	for i, o := range infos {
		keys[i] = o.Key
	}
	return keys
}

// Store is a minimal object store. Keys are generated internal identifiers
// using "/" separators (see OriginalKey and friends), never user filenames.
type Store interface {
	// Put stores r under key, replacing any existing object. size is the
	// content length if known, or -1.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens the object. The caller must close the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List returns all objects whose key has the given prefix, sorted by key.
	// Modified is the zero time when the backend cannot report it.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Ping verifies the backend is reachable.
	Ping(ctx context.Context) error
}

func FolderPrefix(folderID string) string { return fmt.Sprintf("folders/%s/", folderID) }

func OriginalKey(folderID, imageID string) string {
	return fmt.Sprintf("folders/%s/images/%s/original", folderID, imageID)
}

func PreviewKey(folderID, imageID string) string {
	return fmt.Sprintf("folders/%s/images/%s/preview", folderID, imageID)
}

func ThumbnailKey(folderID, imageID string) string {
	return fmt.Sprintf("folders/%s/images/%s/thumbnail", folderID, imageID)
}

// ValidateKey rejects keys that could escape the storage root.
func ValidateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.ContainsRune(key, 0) {
		return fmt.Errorf("storage: invalid key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("storage: invalid key %q", key)
		}
	}
	return nil
}
