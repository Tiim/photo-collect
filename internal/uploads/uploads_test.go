package uploads_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/images/imagetest"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/storage/filesystem"
	"github.com/tiim/photo-collect/internal/uploads"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	db     *database.DB
	svc    *uploads.Service
	folder string
	ids    map[string]string // filename -> image id
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
	store, err := filesystem.New(filepath.Join(dir, "photos"))
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.New(db, log)
	(&jobs.Handlers{DB: db, Store: store, Processor: images.NewGoProcessor(40, 80, 1), Queue: queue, ExportDir: dir, Log: log}).Register(queue)
	go queue.Run(ctx, 2)

	f, err := db.Q.CreateFolder(ctx, sqlc.CreateFolderParams{ID: domain.NewID(), Name: "party"})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		t: t, ctx: ctx, db: db, folder: f.ID, ids: map[string]string{},
		svc: uploads.New(db, store, queue, uploads.Limits{MaxFileSize: 10 << 20, MaxImagesPerFolder: 100, MaxPixels: 60_000_000}, log),
	}
}

func (f *fixture) upload(nick, name string, data []byte) {
	f.t.Helper()
	img, err := f.svc.Ingest(f.ctx, f.folder, nick, name, bytes.NewReader(data))
	if err != nil {
		f.t.Fatalf("ingest %s: %v", name, err)
	}
	f.ids[name] = img.ID
}

// waitAnalyzed blocks until every uploaded image has been scanned for a QR code.
func (f *fixture) waitAnalyzed() {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var pending int
		if err := f.db.QueryRow("SELECT COUNT(*) FROM images WHERE qr_scanned = 0").Scan(&pending); err != nil {
			f.t.Fatal(err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%d images were never analyzed", pending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fixture) image(name string) sqlc.Image {
	f.t.Helper()
	img, err := f.db.Q.GetImage(f.ctx, f.ids[name])
	if err != nil {
		f.t.Fatal(err)
	}
	return img
}

func (f *fixture) wantOffset(name string, want *int64) {
	f.t.Helper()
	got := f.image(name).TimeOffsetSeconds
	switch {
	case want == nil && got.Valid:
		f.t.Errorf("%s: offset = %d, want none", name, got.Int64)
	case want != nil && (!got.Valid || got.Int64 != *want):
		f.t.Errorf("%s: offset = %v, want %d", name, got, *want)
	}
}

func wall(s string) time.Time {
	t, err := time.ParseInLocation(images.WallTimeLayout, s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func secs(n int64) *int64 { return &n }

func clock(w string) images.ClockReading {
	return images.ClockReading{UTC: wall(w), Wall: wall(w)}
}

var (
	iphone = func(taken string) imagetest.Camera {
		return imagetest.Camera{Make: "Apple", Model: "iPhone 14", Taken: wall(taken)}
	}
	pixel = func(taken string) imagetest.Camera {
		return imagetest.Camera{Make: "Google", Model: "Pixel 8", Taken: wall(taken)}
	}
)

func TestCalibrationCorrectsSameDeviceOnly(t *testing.T) {
	f := newFixture(t)

	// Tim's phone runs 2h05m slow. Ordinary photos arrive *before* the
	// calibration shots: the order of uploads must not matter.
	f.upload("Tim", "a.jpg", imagetest.JPEG(64, 48, iphone("2026-09-26T10:00:00")))
	f.upload("Tim", "b.jpg", imagetest.JPEG(64, 48, iphone("2026-09-26T12:00:00")))
	f.upload("Tim", "e.jpg", imagetest.JPEG(64, 48, iphone("2026-09-26T14:30:00")))
	// Same model but another person, and a different camera without any calibration.
	// (Distinct pixel size so this isn't byte-identical to a.jpg: they're
	// different people's photos that merely share a timestamp and model.)
	f.upload("Bob", "bob.jpg", imagetest.JPEG(65, 48, iphone("2026-09-26T10:00:00")))
	f.upload("Tim", "pixel.jpg", imagetest.JPEG(64, 48, pixel("2026-09-26T10:00:00")))
	f.upload("Tim", "noexif.jpg", imagetest.PlainJPEG(64, 48))

	// Calibration shot 1: the phone said 10:30:00 while the page showed 12:35:00.
	f.upload("Tim", "c.jpg", imagetest.ClockPhoto(clock("2026-09-26T12:35:00"), iphone("2026-09-26T10:30:00")))
	// Calibration shot 2, later: the clock was changed to be only 2h slow.
	f.upload("Tim", "d.jpg", imagetest.ClockPhoto(clock("2026-09-26T17:00:00"), iphone("2026-09-26T15:00:00")))
	f.waitAnalyzed()

	f.wantOffset("a.jpg", secs(7500)) // nearest shot: c
	f.wantOffset("b.jpg", secs(7500)) // nearest shot: c
	f.wantOffset("c.jpg", secs(7500))
	f.wantOffset("e.jpg", secs(7200)) // nearest shot: d
	f.wantOffset("d.jpg", secs(7200))
	f.wantOffset("bob.jpg", nil)
	f.wantOffset("pixel.jpg", nil)
	f.wantOffset("noexif.jpg", nil)

	for name, want := range map[string]bool{"c.jpg": true, "d.jpg": true, "a.jpg": false, "bob.jpg": false} {
		img := f.image(name)
		if (img.IsCalibration == 1) != want {
			t.Errorf("%s: is_calibration = %d, want %v", name, img.IsCalibration, want)
		}
		tags, err := f.db.Q.ListImageTags(f.ctx, img.ID)
		if err != nil {
			t.Fatal(err)
		}
		has := false
		for _, tg := range tags {
			has = has || tg.Name == domain.CalibrationTag
		}
		if has != want {
			t.Errorf("%s: calibration tag = %v, want %v", name, has, want)
		}
	}

	// Calibration shots are left out of "export everything".
	exp, err := f.db.Q.CreateExport(f.ctx, sqlc.CreateExportParams{ID: "x", FolderID: f.folder, ExpiresAt: "9999"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Q.AddAllFolderImagesToExport(f.ctx, sqlc.AddAllFolderImagesToExportParams{ExportID: exp.ID, FolderID: f.folder}); err != nil {
		t.Fatal(err)
	}
	exported, err := f.db.Q.ListExportImages(f.ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) != 6 {
		t.Errorf("exported %d images, want 6 (8 minus 2 calibration shots)", len(exported))
	}
}

func TestCalibrationWithoutExifTimeIsHarmless(t *testing.T) {
	f := newFixture(t)
	f.upload("Tim", "a.jpg", imagetest.JPEG(64, 48, iphone("2026-09-26T10:00:00")))
	// The QR code is readable but the photo carries no capture time.
	f.upload("Tim", "c.jpg", imagetest.ClockPhoto(clock("2026-09-26T12:35:00"), imagetest.Camera{Make: "Apple", Model: "iPhone 14"}))
	f.waitAnalyzed()

	if !f.image("c.jpg").CalibRefTime.Valid {
		t.Error("calibration shot not recognised")
	}
	f.wantOffset("a.jpg", nil)
	f.wantOffset("c.jpg", nil)
}

// A byte-identical re-upload into the same folder is skipped: no second
// image row is created and the existing image is handed back, so a guest
// who double-uploads the same photo notices nothing.
func TestExactDuplicateUploadIsSkipped(t *testing.T) {
	f := newFixture(t)
	data := imagetest.JPEG(64, 48, iphone("2026-09-26T10:00:00"))

	first, err := f.svc.Ingest(f.ctx, f.folder, "Tim", "a.jpg", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Ingest(f.ctx, f.folder, "Tim", "a-copy.jpg", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("skip returned a different image: %s vs %s", second.ID, first.ID)
	}
	count, err := f.db.Q.CountImagesInFolder(f.ctx, f.folder)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("images in folder = %d, want 1", count)
	}

	// A genuinely different photo, and the same bytes in a different folder,
	// are unaffected (per-folder scope only).
	other, err := f.db.Q.CreateFolder(f.ctx, sqlc.CreateFolderParams{ID: domain.NewID(), Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := f.svc.Ingest(f.ctx, other.ID, "Tim", "a.jpg", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere.ID == first.ID {
		t.Fatal("upload into a different folder must not be skipped")
	}
}

// Concurrent uploads of the same file into the same folder must still
// result in exactly one image row: the skip has to be race-safe, not just
// correct for sequential requests.
func TestExactDuplicateUploadRaceIsSafe(t *testing.T) {
	f := newFixture(t)
	data := imagetest.JPEG(64, 48, iphone("2026-09-26T10:00:00"))
	const n = 8

	results := make([]*sqlc.Image, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.svc.Ingest(f.ctx, f.folder, "Tim", "a.jpg", bytes.NewReader(data))
		}(i)
	}
	wg.Wait()

	var id string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
		if id == "" {
			id = results[i].ID
		} else if results[i].ID != id {
			t.Fatalf("upload %d returned %s, want %s", i, results[i].ID, id)
		}
	}
	count, err := f.db.Q.CountImagesInFolder(f.ctx, f.folder)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("images in folder = %d, want 1", count)
	}
}

func TestRecomputeIsScopedToFolder(t *testing.T) {
	f := newFixture(t)
	other, err := f.db.Q.CreateFolder(f.ctx, sqlc.CreateFolderParams{ID: domain.NewID(), Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	f.upload("Tim", "a.jpg", imagetest.JPEG(64, 48, iphone("2026-09-26T10:00:00")))
	f.upload("Tim", "c.jpg", imagetest.ClockPhoto(clock("2026-09-26T12:00:00"), iphone("2026-09-26T10:00:00")))
	f.waitAnalyzed()

	// The same device in another folder is not affected by this folder's calibration.
	img, err := f.svc.Ingest(f.ctx, other.ID, "Tim", "z.jpg", bytes.NewReader(imagetest.JPEG(64, 48, iphone("2026-09-26T10:00:00"))))
	if err != nil {
		t.Fatal(err)
	}
	f.ids["z.jpg"] = img.ID
	f.waitAnalyzed()
	f.wantOffset("a.jpg", secs(7200))
	f.wantOffset("z.jpg", nil)
}
