// Package webdav implements storage.Store on top of a WebDAV server such as a
// Hetzner Storage Box.
package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	pathpkg "path"
	"sort"
	"strings"
	"time"

	"github.com/studio-b12/gowebdav"

	"github.com/tiim/photo-collect/internal/storage"
)

const (
	maxAttempts = 3
	retryDelay  = 300 * time.Millisecond
)

type Options struct {
	URL      string
	User     string
	Password string
	// BasePath is the remote directory all keys live under. Defaults to "/".
	BasePath string
}

type Store struct {
	c    *gowebdav.Client
	base string // "" for the server root, otherwise "/dir/sub" (no trailing slash)
}

// New connects to the WebDAV server and makes sure BasePath exists.
func New(ctx context.Context, o Options) (*Store, error) {
	if o.URL == "" {
		return nil, errors.New("webdav: URL is required")
	}
	c := gowebdav.NewClient(strings.TrimRight(o.URL, "/"), o.User, o.Password)
	c.SetTransport(&http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxConnsPerHost:       8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	})
	base := strings.Trim(o.BasePath, "/")
	if base != "" {
		base = "/" + base
	}
	s := &Store{c: c, base: base}
	if err := s.Ping(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) remote(key string) string { return s.base + "/" + key }

func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// OPTIONS alone succeeds on servers without WebDAV enabled, so probe with
	// PROPFIND (Stat) on the base path instead.
	// Directories are probed with a trailing slash: servers like Hetzner answer
	// a bare directory path with a 301, which the client follows as a plain GET.
	root := s.base + "/"
	err := retry(ctx, func() error { _, err := s.c.Stat(root); return err })
	if gowebdav.IsErrNotFound(err) && s.base != "" {
		err = retry(ctx, func() error { return s.c.MkdirAll(s.base, 0o755) })
	}
	if err != nil {
		return fmt.Errorf("webdav: probe %s (is WebDAV enabled for this account?): %w", root, err)
	}
	return nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// WebDAV has no content-type on PUT; downloads are proxied and typed by the app.
	p := s.remote(key)
	// Uploads are only retried when the reader can be rewound.
	if sk, ok := r.(io.ReadSeeker); ok {
		return retry(ctx, func() error {
			if _, err := sk.Seek(0, io.SeekStart); err != nil {
				return err
			}
			return s.c.WriteStream(p, sk, 0o644)
		})
	}
	if size >= 0 {
		// Streams the body without buffering it in memory.
		return s.c.WriteStreamWithLength(p, r, size, 0o644)
	}
	// Unknown length: the library buffers the body to determine it.
	return s.c.WriteStream(p, r, 0o644)
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, err
	}
	var rc io.ReadCloser
	err := retry(ctx, func() (err error) {
		rc, err = s.c.ReadStream(s.remote(key))
		return err
	})
	if gowebdav.IsErrNotFound(err) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("webdav: get %s: %w", key, err)
	}
	return rc, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	// gowebdav treats 404 as success.
	if err := retry(ctx, func() error { return s.c.Remove(s.remote(key)) }); err != nil {
		return fmt.Errorf("webdav: delete %s: %w", key, err)
	}
	return nil
}

// List returns all keys with the given prefix. The prefix need not end at a
// directory boundary; the walk starts at its deepest complete directory.
func (s *Store) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	start := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		start = prefix[:i]
	}
	var objs []storage.ObjectInfo
	var walk func(dir string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var infos []os.FileInfo
		err := retry(ctx, func() (err error) {
			infos, err = s.c.ReadDir(s.base + "/" + dir)
			return err
		})
		if gowebdav.IsErrNotFound(err) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("webdav: list %s: %w", dir, err)
		}
		for _, fi := range infos {
			key := pathpkg.Join(dir, fi.Name())
			if dir == "" {
				key = fi.Name()
			}
			if fi.IsDir() {
				// Only descend into directories that can still contain matches.
				if strings.HasPrefix(key+"/", prefix) || strings.HasPrefix(prefix, key+"/") {
					if err := walk(key); err != nil {
						return err
					}
				}
				continue
			}
			if strings.HasPrefix(key, prefix) {
				objs = append(objs, storage.ObjectInfo{Key: key, Modified: fi.ModTime(), Size: fi.Size()})
			}
		}
		return nil
	}
	if err := walk(start); err != nil {
		return nil, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	return objs, nil
}

// retry runs fn up to maxAttempts times, backing off on transient failures
// (network errors, 429 and 5xx). Other errors are returned immediately.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(); err == nil || attempt == maxAttempts || !transient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * retryDelay):
		}
	}
}

func transient(err error) bool {
	var pe *os.PathError
	if errors.As(err, &pe) {
		var se gowebdav.StatusError
		if errors.As(pe.Err, &se) {
			return se.Status == http.StatusTooManyRequests || se.Status >= 500
		}
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}
