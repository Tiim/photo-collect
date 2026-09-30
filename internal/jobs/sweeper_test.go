package jobs_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/storage"
	"github.com/tiim/photo-collect/internal/storage/filesystem"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// noAgeStore hides object ages, like a backend that cannot report them.
type noAgeStore struct{ storage.Store }

func (s noAgeStore) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	objs, err := s.Store.List(ctx, prefix)
	for i := range objs {
		objs[i].Modified = time.Time{}
	}
	return objs, err
}

type sweepEnv struct {
	t     *testing.T
	ctx   context.Context
	db    *database.DB
	q     *jobs.Queue
	dir   string
	store storage.Store
	logs  *syncBuffer
	qs    *sqlc.Queries
	putFn func(key string, age time.Duration)
}

func newSweep(t *testing.T, wrap func(storage.Store) storage.Store) *sweepEnv {
	db := open(t)
	dir := filepath.Join(t.TempDir(), "photos")
	fsStore, err := filesystem.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var store storage.Store = fsStore
	if wrap != nil {
		store = wrap(store)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	q := jobs.New(db, log)
	(&jobs.Handlers{DB: db, Store: store, Queue: q, Log: log}).Register(q)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go q.Run(ctx, 1)
	e := &sweepEnv{t: t, ctx: ctx, db: db, q: q, dir: dir, store: store, logs: logs, qs: db.Q}
	e.putFn = func(key string, age time.Duration) {
		if err := fsStore.Put(ctx, key, strings.NewReader("data"), 4, ""); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(key)), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *sweepEnv) folder(id string) {
	if _, err := e.qs.CreateFolder(e.ctx, sqlc.CreateFolderParams{ID: id, Name: id}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *sweepEnv) image(folder, id string) {
	_, err := e.qs.InsertImage(e.ctx, sqlc.InsertImageParams{
		ID: id, FolderID: folder, OriginalFilename: id + ".jpg", MimeType: "image/jpeg", SizeBytes: 4,
		Width: 1, Height: 1, Sha256: strings.Repeat(id, 64)[:64], UploaderNickname: "n",
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *sweepEnv) exists(key string) bool {
	_, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(key)))
	return err == nil
}

// sweep runs one sweep job and returns its final status.
func (e *sweepEnv) sweep() string {
	if err := e.q.EnqueueSweep(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := e.db.QueryRow("SELECT status FROM jobs WHERE type = 'sweep_orphans' ORDER BY id DESC LIMIT 1").Scan(&status); err == nil && (status == "done" || status == "failed") {
			return status
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("sweep did not finish")
	return ""
}

func TestSweepDeletesOnlyOldUnreferencedObjects(t *testing.T) {
	e := newSweep(t, nil)
	const old = 48 * time.Hour
	e.folder("f1")
	e.image("f1", "kept")
	e.image("f1", "trashed")
	if _, err := e.qs.TrashImages(e.ctx, sqlc.TrashImagesParams{
		DeletedAt: toNull("2020-01-01T00:00:00.000Z"), FolderID: "f1", ImageIds: []string{"trashed"},
	}); err != nil {
		t.Fatal(err)
	}
	// A folder that is being deleted: left to delete_folder.
	e.folder("f2")
	if _, err := e.qs.SoftDeleteFolder(e.ctx, sqlc.SoftDeleteFolderParams{DeletedAt: toNull("2020-01-01T00:00:00.000Z"), ID: "f2"}); err != nil {
		t.Fatal(err)
	}

	keep := []string{
		storage.OriginalKey("f1", "kept"), storage.PreviewKey("f1", "kept"),
		storage.OriginalKey("f1", "trashed"), storage.ThumbnailKey("f1", "trashed"), // trashed rows exist
		storage.OriginalKey("f1", "young-orphan"), // in-flight upload: younger than the grace period
		storage.OriginalKey("f2", "whatever"),     // folder being deleted
		"folders/f1/other/file",                   // not an image object
		"exports/x.zip",                           // outside folders/
	}
	remove := []string{
		storage.OriginalKey("f1", "old-orphan"), storage.PreviewKey("f1", "old-orphan"),
		storage.OriginalKey("ghost-folder", "img"),
	}
	for _, k := range keep {
		e.putFn(k, old)
	}
	e.putFn(storage.OriginalKey("f1", "young-orphan"), time.Hour)
	for _, k := range remove {
		e.putFn(k, old)
	}

	if st := e.sweep(); st != "done" {
		t.Fatalf("status %s\n%s", st, e.logs)
	}
	for _, k := range keep {
		if !e.exists(k) {
			t.Errorf("%s was deleted", k)
		}
	}
	for _, k := range remove {
		if e.exists(k) {
			t.Errorf("%s was not deleted", k)
		}
	}
	if !strings.Contains(e.logs.String(), "orphan sweep: deleted object") || !strings.Contains(e.logs.String(), "old-orphan") {
		t.Errorf("deletions not logged:\n%s", e.logs)
	}
	// Idempotent: a second run has nothing to do.
	if st := e.sweep(); st != "done" {
		t.Fatalf("second run: %s", st)
	}
}

func TestSweepReportsImagesWithoutOriginal(t *testing.T) {
	e := newSweep(t, nil)
	e.folder("f1")
	e.image("f1", "lost")
	e.image("f1", "fine")
	e.putFn(storage.OriginalKey("f1", "fine"), time.Minute)
	if st := e.sweep(); st != "done" {
		t.Fatal(st)
	}
	if !strings.Contains(e.logs.String(), "images without a stored original") || !strings.Contains(e.logs.String(), "lost") {
		t.Fatalf("missing original not reported:\n%s", e.logs)
	}
	if strings.Contains(e.logs.String(), "fine]") {
		t.Fatalf("image with an original reported:\n%s", e.logs)
	}
}

func TestSweepGuardAbortsWhenMostObjectsAreUnreferenced(t *testing.T) {
	e := newSweep(t, nil)
	e.folder("f1")
	e.image("f1", "real")
	e.putFn(storage.OriginalKey("f1", "real"), 48*time.Hour)
	var orphans []string
	for i := 0; i < 120; i++ {
		k := storage.OriginalKey("f1", fmt.Sprintf("o%03d", i))
		e.putFn(k, 48*time.Hour)
		orphans = append(orphans, k)
	}
	if st := e.sweep(); st != "failed" {
		t.Fatalf("status %s, want failed", st)
	}
	for _, k := range orphans {
		if !e.exists(k) {
			t.Fatalf("%s deleted despite the guard", k)
		}
	}
	var attempts int
	e.db.QueryRow("SELECT attempts FROM jobs WHERE type = 'sweep_orphans'").Scan(&attempts)
	if attempts != 1 {
		t.Errorf("guard failure was retried (%d attempts)", attempts)
	}
	if !strings.Contains(e.logs.String(), "database pointed at the right storage") {
		t.Errorf("guard not explained:\n%s", e.logs)
	}
}

func TestSweepBelowGuardThresholdRuns(t *testing.T) {
	e := newSweep(t, nil)
	e.folder("f1")
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("i%03d", i)
		e.image("f1", id)
		e.putFn(storage.OriginalKey("f1", id), 48*time.Hour)
	}
	// 30 of 230 objects (13 %) are orphans: under the 20 % guard.
	for i := 0; i < 30; i++ {
		e.putFn(storage.OriginalKey("f1", fmt.Sprintf("o%03d", i)), 48*time.Hour)
	}
	if st := e.sweep(); st != "done" {
		t.Fatalf("status %s\n%s", st, e.logs)
	}
	if e.exists(storage.OriginalKey("f1", "o000")) {
		t.Fatal("orphan kept")
	}
}

func TestSweepSkipsObjectsWithoutAge(t *testing.T) {
	e := newSweep(t, func(s storage.Store) storage.Store { return noAgeStore{s} })
	e.folder("f1")
	e.putFn(storage.OriginalKey("f1", "old-orphan"), 48*time.Hour)
	if st := e.sweep(); st != "done" {
		t.Fatal(st)
	}
	if !e.exists(storage.OriginalKey("f1", "old-orphan")) {
		t.Fatal("object deleted although its age is unknown")
	}
	if !strings.Contains(e.logs.String(), "does not report object age") {
		t.Fatalf("no warning:\n%s", e.logs)
	}
}

func TestEnqueueSweepIsNotDuplicated(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 0; i < 3; i++ {
		if err := q.EnqueueSweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM jobs WHERE type = 'sweep_orphans'").Scan(&n)
	if n != 1 {
		t.Fatalf("%d sweep jobs queued, want 1", n)
	}
}

func toNull(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

func TestDeleteImageObjectsSkipsExistingImage(t *testing.T) {
	e := newSweep(t, nil)
	e.folder("f1")
	e.image("f1", "live")
	e.image("f1", "x")
	e.putFn(storage.OriginalKey("f1", "live"), time.Minute)
	e.putFn(storage.OriginalKey("f1", "gone"), time.Minute)
	for _, id := range []string{"live", "gone"} {
		if err := e.q.Enqueue(e.ctx, e.qs, jobs.TypeDeleteImageObjects, jobs.DeleteImageObjectsPayload{FolderID: "f1", ImageID: id}); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, func() bool { return !e.exists(storage.OriginalKey("f1", "gone")) })
	time.Sleep(200 * time.Millisecond)
	if !e.exists(storage.OriginalKey("f1", "live")) {
		t.Fatal("objects of an existing image were deleted")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}
