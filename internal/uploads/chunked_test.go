package uploads

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestChunksSweepDiscardsAbandonedUploads(t *testing.T) {
	var logs bytes.Buffer
	c, err := NewChunks(t.TempDir(), 2, time.Hour, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.now = func() time.Time { return now }

	a, _ := c.Start("tok", "f", "Bob", "a.jpg", 4)
	b, _ := c.Start("tok", "f", "Bob", "b.jpg", 4)
	if _, err := c.Start("tok", "f", "Bob", "c.jpg", 4); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("third pending upload: %v", err)
	}
	if _, err := c.Write(a, 0, strings.NewReader("abcd"), nil); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := c.Complete(a, func(r io.Reader, _ Stats) error { d, _ := io.ReadAll(r); got = string(d); return nil }); err != nil || got != "abcd" {
		t.Fatalf("complete: %v %q", err, got)
	}
	if _, err := os.Stat(a.path); !os.IsNotExist(err) {
		t.Errorf("completed upload left its file: %v", err)
	}
	// A finished upload no longer counts as pending.
	if _, err := c.Start("tok", "f", "Bob", "c.jpg", 4); err != nil {
		t.Fatalf("start after completion: %v", err)
	}

	now = now.Add(30 * time.Minute)
	if _, err := c.Get(b.ID, "tok"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(45 * time.Minute) // b was used 45 min ago, the others 75 min ago
	c.Sweep()
	if _, err := c.Get(a.ID, "tok"); !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("old upload kept: %v", err)
	}
	if _, err := c.Get(b.ID, "tok"); err != nil {
		t.Errorf("recently used upload discarded: %v", err)
	}
	if _, err := c.Get(b.ID, "other"); !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("upload reachable with another token: %v", err)
	}
	// Only the unfinished one is reported (c.jpg, nothing received).
	if n := strings.Count(logs.String(), "idle upload discarded"); n != 1 || !strings.Contains(logs.String(), "received_bytes=0 size=4") {
		t.Errorf("sweep log (%d):\n%s", n, logs.String())
	}
	entries, _ := os.ReadDir(c.dir)
	if len(entries) != 1 {
		t.Errorf("files left: %d", len(entries))
	}
}

func TestChunksAbortUnblocksStuckWriter(t *testing.T) {
	c, err := NewChunks(t.TempDir(), 2, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Start("tok", "f", "Bob", "a.jpg", 6)
	pr, pw := io.Pipe()
	done := make(chan error)
	go func() {
		_, err := c.Write(p, 0, pr, func() { pr.CloseWithError(errors.New("aborted")) })
		done <- err
	}()
	pw.Write([]byte("abc"))
	// The second writer cuts the stuck one off and continues where it ended.
	off, err := c.Write(p, 0, strings.NewReader("abcdef"), nil)
	if !errors.Is(err, ErrOffsetMismatch) || off != 3 {
		t.Fatalf("second writer: %d %v", off, err)
	}
	if err := <-done; err == nil {
		t.Error("stuck writer reported success")
	}
	if off, err := c.Write(p, 3, strings.NewReader("def"), nil); err != nil || off != 6 {
		t.Fatalf("rest: %d %v", off, err)
	}
	if st := p.Stats(); st.Chunks != 1 || st.Incomplete != 1 || st.OffsetMismatches != 1 || st.Received != 6 {
		t.Errorf("stats: %+v", st)
	}
}
