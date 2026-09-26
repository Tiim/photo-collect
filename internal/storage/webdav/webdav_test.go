package webdav_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	xwebdav "golang.org/x/net/webdav"

	"github.com/tiim/photo-collect/internal/storage"
	"github.com/tiim/photo-collect/internal/storage/storagetest"
	"github.com/tiim/photo-collect/internal/storage/webdav"
)

func newServer(t *testing.T, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	var h http.Handler = &xwebdav.Handler{FileSystem: xwebdav.NewMemFS(), LockSystem: xwebdav.NewMemLS()}
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestContract(t *testing.T) {
	srv := newServer(t, nil)
	s, err := webdav.New(context.Background(), webdav.Options{URL: srv.URL, BasePath: "/photos/data"})
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, s)
}

func TestContractRoot(t *testing.T) {
	srv := newServer(t, nil)
	s, err := webdav.New(context.Background(), webdav.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, s)
}

func TestSpecialCharsAndUnknownSize(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t, nil)
	s, err := webdav.New(ctx, webdav.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	key := "folders/f 1/images/a+b%20c/original"
	data := bytes.Repeat([]byte("z"), 5000)
	// Non-seekable reader with unknown size.
	if err := s.Put(ctx, key, io.MultiReader(bytes.NewReader(data)), -1, ""); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatal("content mismatch")
	}
	keys, err := s.List(ctx, "folders/f 1/")
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("list = %v, %v", keys, err)
	}
}

func TestRetryOnServerError(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Int32
	srv := newServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && fail.Add(-1) >= 0 {
				http.Error(w, "boom", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	s, err := webdav.New(ctx, webdav.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "a/b", bytes.NewReader([]byte("hi")), 2, ""); err != nil {
		t.Fatal(err)
	}
	fail.Store(2)
	rc, err := s.Get(ctx, "a/b")
	if err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	rc.Close()
	fail.Store(100)
	if _, err := s.Get(ctx, "a/b"); err == nil || err == storage.ErrNotFound {
		t.Fatalf("expected persistent error, got %v", err)
	}
}

func TestBadCredentials(t *testing.T) {
	srv := newServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, p, ok := r.BasicAuth(); !ok || p != "secret" {
				w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	if _, err := webdav.New(context.Background(), webdav.Options{URL: srv.URL, User: "u", Password: "wrong"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := webdav.New(context.Background(), webdav.Options{URL: srv.URL, User: "u", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
}

// Runs against a real server, e.g. a Hetzner Storage Box:
//
//	TEST_WEBDAV_URL=https://uXXXX.your-storagebox.de TEST_WEBDAV_USER=... TEST_WEBDAV_PASSWORD=... go test ./internal/storage/webdav
func TestContractReal(t *testing.T) {
	url := os.Getenv("TEST_WEBDAV_URL")
	if url == "" {
		t.Skip("TEST_WEBDAV_URL not set")
	}
	s, err := webdav.New(context.Background(), webdav.Options{
		URL: url, User: os.Getenv("TEST_WEBDAV_USER"), Password: os.Getenv("TEST_WEBDAV_PASSWORD"),
		BasePath: "/photo-collect-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, s)
}
