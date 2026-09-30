package downloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/images/imagetest"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/storage"
	"github.com/tiim/photo-collect/internal/storage/filesystem"
	"github.com/tiim/photo-collect/internal/uploads"
)

// gateStore blocks reads of originals until released, so a build stays "running".
type gateStore struct {
	storage.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Store.Get(ctx, key)
}

type fixture struct {
	ctx    context.Context
	db     *database.DB
	svc    *Service
	queue  *jobs.Queue
	gate   *gateStore
	folder string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fs, err := filesystem.New(filepath.Join(dir, "photos"))
	if err != nil {
		t.Fatal(err)
	}
	gate := &gateStore{Store: fs, started: make(chan struct{}), release: make(chan struct{})}
	queue := jobs.New(db, log)
	svc, err := New(db, gate, queue, filepath.Join(dir, "exports"), time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := db.Q.CreateFolder(ctx, sqlc.CreateFolderParams{ID: domain.NewID(), Name: "party"})
	if err != nil {
		t.Fatal(err)
	}
	up := uploads.New(db, fs, queue, uploads.Limits{MaxFileSize: 10 << 20, MaxImagesPerFolder: 100, MaxPixels: 60_000_000}, log)
	for _, n := range []string{"a.jpg", "b.jpg"} {
		if _, err := up.Ingest(ctx, folder.ID, "anna", n, bytes.NewReader(imagetest.PlainJPEG(60, 40))); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{ctx: ctx, db: db, svc: svc, queue: queue, gate: gate, folder: folder.ID}
}

func (f *fixture) status(id string) string {
	e, err := f.db.Q.GetExport(f.ctx, id)
	if err != nil {
		return err.Error()
	}
	return e.Status
}

func TestCreateRefusesTooLargeExport(t *testing.T) {
	f := newFixture(t)
	f.svc.SetLimits(Limits{MaxConcurrent: 1, MaxBytes: 10}) // any real JPEG is bigger
	_, err := f.svc.Create(f.ctx, f.folder, "", nil)
	var tl *TooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("err = %v, want TooLargeError", err)
	}
	var n int
	f.db.QueryRow("SELECT COUNT(*) FROM exports").Scan(&n)
	if n != 0 {
		t.Errorf("refused export left %d rows behind", n)
	}
	f.svc.SetLimits(Limits{MaxConcurrent: 1, MaxBytes: 1 << 20})
	if _, err := f.svc.Create(f.ctx, f.folder, "", nil); err != nil {
		t.Errorf("export within the limit refused: %v", err)
	}
}

func TestBuildRefusesTooLargeExport(t *testing.T) {
	f := newFixture(t)
	exp, err := f.svc.Create(f.ctx, f.folder, "", nil) // created while unlimited
	if err != nil {
		t.Fatal(err)
	}
	f.svc.SetLimits(Limits{MaxConcurrent: 1, MaxBytes: 10}) // limit lowered afterwards
	err = f.svc.build(f.ctx, mustJSON(t, jobs.ExportPayload{ExportID: exp.ID}))
	if err == nil || f.status(exp.ID) != "failed" {
		t.Fatalf("err = %v, status = %s", err, f.status(exp.ID))
	}
}

func TestOnlyConfiguredNumberOfExportsBuildAtOnce(t *testing.T) {
	f := newFixture(t)
	old := busyRetry
	busyRetry = 20 * time.Millisecond
	t.Cleanup(func() { busyRetry = old })

	f.svc.SetLimits(Limits{MaxConcurrent: 1})
	e1, err := f.svc.Create(f.ctx, f.folder, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := f.svc.Create(f.ctx, f.folder, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	go f.queue.Run(f.ctx, 2)

	<-f.gate.started
	// Give the second worker time to pick up the other export and be turned away.
	waitUntil(t, "second export deferred", func() bool {
		var attempts int
		var status string
		f.db.QueryRow("SELECT status, attempts FROM jobs WHERE type = ? AND payload LIKE ?", jobs.TypeBuildExport, "%"+e2.ID+"%").Scan(&status, &attempts)
		return status == "pending" && attempts == 0 || status == "running" && attempts == 1
	})
	running := 0
	for _, id := range []string{e1.ID, e2.ID} {
		if f.status(id) == "running" {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("%d exports running with MaxConcurrent=1", running)
	}

	close(f.gate.release)
	waitUntil(t, "both exports ready", func() bool { return f.status(e1.ID) == "ready" && f.status(e2.ID) == "ready" })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
