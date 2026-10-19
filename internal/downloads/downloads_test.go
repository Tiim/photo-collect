package downloads

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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

type fixture struct {
	t      *testing.T
	ctx    context.Context
	db     *database.DB
	store  storage.Store
	svc    *Service
	up     *uploads.Service
	folder string
	data   map[string][]byte // image ID -> original bytes
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
	folder, err := db.Q.CreateFolder(ctx, sqlc.CreateFolderParams{ID: domain.NewID(), Name: "party"})
	if err != nil {
		t.Fatal(err)
	}
	up := uploads.New(db, fs, jobs.New(db, log), uploads.Limits{MaxFileSize: 10 << 20, MaxImagesPerFolder: 100, MaxPixels: 60_000_000}, log)
	return &fixture{t: t, ctx: ctx, db: db, store: fs, svc: New(db, fs, log), up: up, folder: folder.ID, data: map[string][]byte{}}
}

// upload stores a distinct JPEG under the given file name and returns its ID.
func (f *fixture) upload(name string) string {
	f.t.Helper()
	data := imagetest.PlainJPEG(60+len(f.data), 40) // distinct bytes, no duplicate skip
	img, err := f.up.Ingest(f.ctx, f.folder, "anna", name, bytes.NewReader(data))
	if err != nil {
		f.t.Fatal(err)
	}
	f.data[img.ID] = data
	return img.ID
}

func (f *fixture) names(ids ...string) map[string]string {
	f.t.Helper()
	exp, err := f.svc.Prepare(f.ctx, f.folder, ids)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, file := range exp.Files {
		out[file.ImageID] = file.Name
	}
	return out
}

func TestStreamedZipHasOriginalsAndSidecars(t *testing.T) {
	f := newFixture(t)
	a := f.upload("a.jpg")
	b := f.upload("a.jpg") // same name, different photo
	if _, err := f.db.Q.SetImageRating(f.ctx, sqlc.SetImageRatingParams{Rating: sql.NullInt64{Int64: 4, Valid: true}, ID: b}); err != nil {
		t.Fatal(err)
	}
	// a has no capture time, b one set by hand.
	if _, err := f.db.Exec("UPDATE images SET manual_time = ? WHERE id = ?", "2025-07-14T23:30:00", b); err != nil {
		t.Fatal(err)
	}
	exp, err := f.svc.Prepare(f.ctx, f.folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.svc.WriteZip(f.ctx, &buf, exp); err != nil {
		t.Fatal(err)
	}
	if got, want := int64(buf.Len()), exp.ZipSize(); got != want {
		t.Errorf("ZIP has %d bytes, ZipSize says %d", got, want)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{
		"party/undated/a.jpg": f.data[a], "party/undated/a.xmp": nil,
		"party/2025-07-14/a_2.jpg": f.data[b], "party/2025-07-14/a_2.xmp": nil,
	}
	if len(zr.File) != len(want) {
		t.Errorf("%d entries, want %d", len(zr.File), len(want))
	}
	for _, zf := range zr.File {
		content, ok := want[zf.Name]
		if !ok {
			t.Errorf("unexpected entry %s", zf.Name)
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc) // also checks the CRC
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", zf.Name, err)
		}
		switch {
		case strings.HasSuffix(zf.Name, ".jpg"):
			if zf.Method != zip.Store || !bytes.Equal(got, content) {
				t.Errorf("%s: method %d, original not byte-identical", zf.Name, zf.Method)
			}
		case zf.Name == "party/2025-07-14/a_2.xmp":
			if !bytes.Contains(got, []byte(`xmp:Rating="4"`)) || !bytes.Contains(got, []byte("anna")) {
				t.Errorf("sidecar of the rated photo:\n%s", got)
			}
		}
	}
}

// Content-Length is announced before streaming, so ZipSize must match the
// writer exactly, also once entries and offsets need ZIP64.
func TestZipSizeMatchesWriter(t *testing.T) {
	cases := map[string][]int64{"empty": {}, "small": {0, 1, 1000}}
	// At 4 GiB - 1 only the central directory needs ZIP64, at 4 GiB the data
	// descriptor too; the entry after it has an offset beyond 4 GiB.
	for _, d := range []int64{-1, 0} {
		cases[fmt.Sprintf("4 GiB %+d", d)] = []int64{5, 1<<32 + d, 7}
	}
	many := make([]int64, 33_000) // over 65535 entries with the sidecars
	cases["many"] = many
	for name, sizes := range cases {
		t.Run(name, func(t *testing.T) {
			exp := &Export{FolderID: "f", Dir: "Fête", Created: time.Now()}
			store := zeroStore{sizes: map[string]int64{}}
			for i, n := range sizes {
				id := fmt.Sprint("img", i)
				var xmp []byte
				if len(sizes) < 100 { // deflate is slow under -race
					xmp = BuildXMP(XMPData{Rating: i % 6, Tags: []string{"x"}})
				}
				store.sizes[storage.OriginalKey(exp.FolderID, id)] = n
				exp.Files = append(exp.Files, File{
					ImageID: id, Name: "IMG_" + strings.Repeat("ä", i%3) + fmt.Sprint(i) + ".jpg", Size: n,
					Modified: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), XMP: xmp,
				})
			}
			var c countWriter
			if err := (&Service{store: store}).WriteZip(context.Background(), &c, exp); err != nil {
				t.Fatal(err)
			}
			if c.n != exp.ZipSize() {
				t.Errorf("wrote %d bytes, ZipSize = %d", c.n, exp.ZipSize())
			}
		})
	}
}

// zeroStore serves originals of the given sizes (by key), all zeros.
type zeroStore struct {
	storage.Store // only Get is used
	sizes         map[string]int64
}

func (z zeroStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	n, ok := z.sizes[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(io.LimitReader(zeros{}, n)), nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestExportNamesAreStable(t *testing.T) {
	f := newFixture(t)
	a := f.upload("IMG.jpg")
	b := f.upload("img.png") // same name without extension, other case
	c := f.upload("other.jpg")
	all := f.names()
	want := map[string]string{a: "IMG.jpg", b: "img_2.png", c: "other.jpg"}
	for id, n := range want {
		if all[id] != n {
			t.Errorf("%s named %q, want %q (all: %v)", id, all[id], n, all)
		}
	}
	// A selection, in any order, does not rename anything.
	if got := f.names(c, b); got[b] != "img_2.png" || got[c] != "other.jpg" || len(got) != 2 {
		t.Errorf("selection: %v", got)
	}
	// New uploads and trashing never rename existing photos.
	d := f.upload("IMG.jpg")
	if _, err := f.db.Q.TrashImages(f.ctx, sqlc.TrashImagesParams{FolderID: f.folder, ImageIds: []string{a}, DeletedAt: sql.NullString{String: "2026-01-01", Valid: true}}); err != nil {
		t.Fatal(err)
	}
	e := f.upload("IMG.jpg")
	got := f.names()
	if got[b] != "img_2.png" || got[c] != "other.jpg" || got[d] != "IMG_3.jpg" || got[e] != "IMG_4.jpg" {
		t.Errorf("after uploads and trash: %v", got)
	}
	if _, ok := got[a]; ok {
		t.Error("trashed photo exported")
	}
}

func TestWriteZipRefusesOriginalOfUnexpectedSize(t *testing.T) {
	f := newFixture(t)
	id := f.upload("a.jpg")
	if _, err := f.db.Exec("UPDATE images SET size_bytes = size_bytes - 1 WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	exp, err := f.svc.Prepare(f.ctx, f.folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.WriteZip(f.ctx, io.Discard, exp); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Errorf("err = %v", err)
	}
}
