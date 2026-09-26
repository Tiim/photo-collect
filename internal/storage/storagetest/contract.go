// Package storagetest provides a contract test suite for storage.Store implementations.
package storagetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/tiim/photo-collect/internal/storage"
)

func Run(t *testing.T, s storage.Store) {
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	t.Run("put get", func(t *testing.T) {
		data := bytes.Repeat([]byte("abc"), 1000)
		if err := s.Put(ctx, "folders/f1/images/i1/original", bytes.NewReader(data), int64(len(data)), "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		rc, err := s.Get(ctx, "folders/f1/images/i1/original")
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, data) {
			t.Fatal("content mismatch")
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		key := "folders/f1/images/i2/thumbnail"
		_ = s.Put(ctx, key, bytes.NewReader([]byte("one")), -1, "")
		_ = s.Put(ctx, key, bytes.NewReader([]byte("two")), -1, "")
		rc, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if string(got) != "two" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("get missing", func(t *testing.T) {
		if _, err := s.Get(ctx, "folders/nope/x"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("list and delete", func(t *testing.T) {
		keys := []string{"folders/f2/images/a/original", "folders/f2/images/a/preview", "folders/f3/images/b/original"}
		for _, k := range keys {
			if err := s.Put(ctx, k, bytes.NewReader([]byte("x")), 1, ""); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.List(ctx, storage.FolderPrefix("f2"))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		if !slices.Equal(got, keys[:2]) {
			t.Fatalf("list = %v", got)
		}
		for _, k := range got {
			if err := s.Delete(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Delete(ctx, "folders/f2/images/a/original"); err != nil {
			t.Fatalf("deleting missing key should succeed: %v", err)
		}
		got, _ = s.List(ctx, storage.FolderPrefix("f2"))
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})

	t.Run("invalid keys", func(t *testing.T) {
		for _, k := range []string{"", "/abs", "a/../b", "a//b", "..", "a/./b"} {
			if err := s.Put(ctx, k, bytes.NewReader(nil), 0, ""); err == nil {
				t.Errorf("Put(%q) should fail", k)
			}
		}
	})
}
