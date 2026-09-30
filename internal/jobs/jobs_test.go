package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/images/imagetest"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/storage"
	"github.com/tiim/photo-collect/internal/storage/filesystem"
)

func open(t *testing.T) *database.DB {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestJobRunsAndCompletes(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan string, 1)
	q.Handle("test", func(ctx context.Context, p json.RawMessage) error { done <- string(p); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	if err := q.Enqueue(ctx, db.Q, "test", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-done:
		if p != `{"n":1}` {
			t.Fatalf("payload %s", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job did not run")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		db.QueryRow("SELECT status FROM jobs").Scan(&status)
		if status == "done" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailedJobIsRescheduled(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls atomic.Int32
	q.Handle("bad", func(ctx context.Context, p json.RawMessage) error { calls.Add(1); return errors.New("boom") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	q.Enqueue(ctx, db.Q, "bad", nil)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var status, errMsg string
		var attempts int
		var runAt string
		db.QueryRow("SELECT status, attempts, run_at, COALESCE(error,'') FROM jobs").Scan(&status, &attempts, &runAt, &errMsg)
		if status == "pending" && attempts == 1 && errMsg == "boom" {
			if runAt <= database.Time(time.Now()) {
				t.Fatalf("retry not delayed: %s", runAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status=%s attempts=%d err=%s", status, attempts, errMsg)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

// ScheduleFolderScan debounces: two calls close together for the same
// folder must leave exactly one pending scan job, with its run_at pushed
// out by the later call rather than a second job being stacked.
func TestScheduleFolderScanDebounces(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := q.ScheduleFolderScan(ctx, db.Q, "f1"); err != nil {
		t.Fatal(err)
	}
	var firstRunAt string
	if err := db.QueryRow(`SELECT run_at FROM jobs WHERE type = 'scan_folder_duplicates'`).Scan(&firstRunAt); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := q.ScheduleFolderScan(ctx, db.Q, "f1"); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = 'scan_folder_duplicates'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("jobs = %d, want 1 (debounced)", count)
	}
	var secondRunAt string
	if err := db.QueryRow(`SELECT run_at FROM jobs WHERE type = 'scan_folder_duplicates'`).Scan(&secondRunAt); err != nil {
		t.Fatal(err)
	}
	if secondRunAt <= firstRunAt {
		t.Fatalf("run_at not pushed out: %s -> %s", firstRunAt, secondRunAt)
	}

	// A different folder gets its own, independent pending job.
	if err := q.ScheduleFolderScan(ctx, db.Q, "f2"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = 'scan_folder_duplicates'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("jobs = %d, want 2 (one per folder)", count)
	}
}

func TestOrphanedRunningJobIsRequeuedOnStart(t *testing.T) {
	db := open(t)
	// Simulate a crash mid-job.
	db.Exec(`INSERT INTO jobs (type, status, attempts, started_at) VALUES ('test', 'running', 1, '2020-01-01T00:00:00.000Z')`)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{}, 1)
	q.Handle("test", func(ctx context.Context, p json.RawMessage) error { done <- struct{}{}; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("orphaned job was not resumed")
	}
}

func TestPermanentErrorSkipsRetries(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls atomic.Int32
	q.Handle("bad", func(ctx context.Context, p json.RawMessage) error {
		calls.Add(1)
		return jobs.Permanent(errors.New("boom"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	q.Enqueue(ctx, db.Q, "bad", nil)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		var attempts int
		db.QueryRow("SELECT status, attempts FROM jobs").Scan(&status, &attempts)
		if status == "failed" {
			if attempts != 1 || calls.Load() != 1 {
				t.Fatalf("attempts = %d, calls = %d, want 1", attempts, calls.Load())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type failingProcessor struct{ images.Processor }

func (failingProcessor) Derive(context.Context, io.Reader, string) (*images.Derived, error) {
	return nil, errors.New("cannot decode")
}

// TestBackfillSkipsUndecodableAndQueuedImages covers the startup pHash
// backfill: an image whose derivation failed permanently is never listed
// again, and one that already has an unfinished derive job is not queued twice.
func TestBackfillSkipsUndecodableAndQueuedImages(t *testing.T) {
	db := open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := db.Exec(`INSERT INTO folders (id, name) VALUES ('f1', 'F')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bad", "queued", "todo"} {
		if _, err := db.Exec(`INSERT INTO images (id, folder_id, original_filename, mime_type, size_bytes, width, height, sha256, uploader_nickname)
			VALUES (?, 'f1', 'x.jpg', 'image/jpeg', 1, 1, 1, ?, 'n')`, id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Put(ctx, storage.OriginalKey("f1", "bad"), strings.NewReader("junk"), 4, "image/jpeg"); err != nil {
		t.Fatal(err)
	}

	q := jobs.New(db, log)
	(&jobs.Handlers{DB: db, Store: store, Processor: failingProcessor{}, Queue: q, Log: log}).Register(q)
	listed := func() []string {
		rows, err := db.Q.ListImagesMissingPHash(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		sort.Strings(ids)
		return ids
	}
	if got := listed(); len(got) != 3 {
		t.Fatalf("initially listed %v, want 3 images", got)
	}

	// Only "bad" is processed: enqueue it, run, and wait for the job to die.
	q.Enqueue(ctx, db.Q, jobs.TypeDeriveImage, jobs.DerivePayload{ImageID: "bad"})
	go q.Run(ctx, 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		var attempts int
		db.QueryRow("SELECT status, attempts FROM jobs").Scan(&status, &attempts)
		if status == "failed" {
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1 (permanent)", attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job status = %q", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := listed(); len(got) != 2 {
		t.Fatalf("after failure listed %v, want queued+todo", got)
	}

	// An unfinished derive job hides "queued" from the backfill.
	if _, err := db.Exec(`INSERT INTO jobs (type, payload, run_at, status) VALUES ('derive_image', '{"image_id":"queued"}', '2999-01-01T00:00:00.000Z', 'pending')`); err != nil {
		t.Fatal(err)
	}
	if got := listed(); len(got) != 1 || got[0] != "todo" {
		t.Fatalf("listed %v, want [todo]", got)
	}
}

func TestDeferDoesNotConsumeAttempts(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls atomic.Int32
	seen := make(chan int, 1)
	q.Handle("busy", func(ctx context.Context, p json.RawMessage) error {
		// More deferrals than the retry limit: the job must still not fail.
		if n := calls.Add(1); n <= 8 {
			return jobs.Defer(0)
		}
		var attempts int
		db.QueryRow("SELECT attempts FROM jobs").Scan(&attempts)
		seen <- attempts
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	q.Enqueue(ctx, db.Q, "busy", nil)
	select {
	case attempts := <-seen:
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1", attempts)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("job did not complete, %d calls", calls.Load())
	}
}

// The GPS backfill looks at each existing image exactly once, whether or not it
// finds a position, and skips images that already have a queued job.
func TestExtractGPSBackfill(t *testing.T) {
	db := open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := db.Exec(`INSERT INTO folders (id, name) VALUES ('f1', 'F')`); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"geo":  imagetest.JPEG(16, 16, imagetest.Camera{Make: "Apple", HasGPS: true, Lat: 47.3769, Lon: 8.5417}),
		"none": imagetest.PlainJPEG(16, 16),
		"junk": []byte("not an image"),
	}
	for id, data := range files {
		if _, err := db.Exec(`INSERT INTO images (id, folder_id, original_filename, mime_type, size_bytes, width, height, sha256, uploader_nickname)
			VALUES (?, 'f1', 'x.jpg', 'image/jpeg', 1, 1, 1, ?, 'n')`, id, id); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, storage.OriginalKey("f1", id), bytes.NewReader(data), int64(len(data)), "image/jpeg"); err != nil {
			t.Fatal(err)
		}
	}

	q := jobs.New(db, log)
	(&jobs.Handlers{DB: db, Store: store, Processor: failingProcessor{}, Queue: q, Log: log}).Register(q)
	missing := func() int {
		ids, err := db.Q.ListImagesMissingGPS(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(ids)
	}
	if n := missing(); n != 3 {
		t.Fatalf("missing = %d, want 3", n)
	}
	ids, _ := db.Q.ListImagesMissingGPS(ctx)
	for _, id := range ids {
		q.Enqueue(ctx, db.Q, jobs.TypeExtractGPS, jobs.ExtractGPSPayload{ImageID: id})
	}
	if n := missing(); n != 0 {
		t.Fatalf("images with a queued job are still listed (%d)", n)
	}
	go q.Run(ctx, 2)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var done int
		db.QueryRow("SELECT COUNT(*) FROM images WHERE gps_attempted_at IS NOT NULL").Scan(&done)
		if done == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d images attempted", done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var lat, lon float64
	if err := db.QueryRow("SELECT gps_lat, gps_lon FROM images WHERE id = 'geo'").Scan(&lat, &lon); err != nil || lat < 47.37 || lat > 47.38 || lon < 8.54 || lon > 8.55 {
		t.Fatalf("geo position = %v, %v (%v)", lat, lon, err)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM images WHERE gps_lat IS NOT NULL").Scan(&n)
	if n != 1 {
		t.Fatalf("%d images have a position, want 1", n)
	}
	// Finished: nothing to backfill on the next start.
	time.Sleep(50 * time.Millisecond)
	if n := missing(); n != 0 {
		t.Fatalf("missing after backfill = %d", n)
	}
}
