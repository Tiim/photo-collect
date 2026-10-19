// Package downloads exports photos: a ZIP of the originals plus XMP sidecars
// streamed straight into the response, and the file list behind "save to folder".
package downloads

import (
	"archive/zip"
	"cmp"
	"compress/flate"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/storage"
)

type Service struct {
	db    *database.DB
	store storage.Store
	log   *slog.Logger
}

func New(db *database.DB, store storage.Store, log *slog.Logger) *Service {
	return &Service{db: db, store: store, log: log}
}

// Export is the content of one export, in upload order.
type Export struct {
	FolderID   string
	FolderName string
	Dir        string // top-level directory in the ZIP and in "save to folder", with a subdirectory per capture date
	Created    time.Time
	Files      []File
}

// File is one exported photo and its sidecar, or a video (without sidecar).
type File struct {
	ImageID  string
	Name     string // stable per image, see EnsureNames
	DateDir  string // capture date (YYYY-MM-DD) or UndatedDir, subdirectory below Export.Dir
	Size     int64
	SHA256   string
	Modified time.Time // upload time
	XMP      []byte    // nil for videos
}

// UndatedDir holds the photos and videos without a known capture time.
const UndatedDir = "undated"

// DateDir is the subdirectory for a capture time: its date as YYYY-MM-DD.
// Capture times are wall-clock times of the camera, so no time zone applies.
func DateDir(captured time.Time, ok bool) string {
	if !ok {
		return UndatedDir
	}
	return captured.Format("2006-01-02")
}

// path is the entry name of name (the file or its sidecar) in the ZIP.
func (e *Export) path(f File, name string) string {
	if f.DateDir == "" {
		return e.Dir + "/" + name
	}
	return e.Dir + "/" + f.DateDir + "/" + name
}

// XMPName is the name of the photo's sidecar: the photo's name with .xmp as
// extension. Export names are unique without extension, so sidecars never collide.
func (f File) XMPName() string {
	return strings.TrimSuffix(f.Name, path.Ext(f.Name)) + ".xmp"
}

// Prepare collects the given images of a folder (all images except
// calibration shots if imageIDs is empty). Trashed images are left out.
func (s *Service) Prepare(ctx context.Context, folderID string, imageIDs []string) (*Export, error) {
	folder, err := s.db.Q.GetFolder(ctx, folderID)
	if err != nil {
		return nil, err
	}
	if err := s.EnsureNames(ctx, folderID); err != nil {
		return nil, fmt.Errorf("assign export names: %w", err)
	}
	var imgs []sqlc.Image
	if len(imageIDs) == 0 {
		if imgs, err = s.db.Q.ListExportableImagesInFolder(ctx, folderID); err != nil {
			return nil, err
		}
	} else {
		// In chunks: "all matching" a filter can be thousands of IDs.
		for ids := range slices.Chunk(imageIDs, 500) {
			part, err := s.db.Q.ListImagesByIDs(ctx, sqlc.ListImagesByIDsParams{FolderID: folderID, ImageIds: ids})
			if err != nil {
				return nil, err
			}
			imgs = append(imgs, part...)
		}
		slices.SortFunc(imgs, func(a, b sqlc.Image) int { return cmp.Compare(a.Seq, b.Seq) })
	}
	tags, err := s.tags(ctx, imgs)
	if err != nil {
		return nil, err
	}
	e := &Export{FolderID: folder.ID, FolderName: folder.Name, Dir: domain.SafeFilename(folder.Name), Created: time.Now()}
	for _, im := range imgs {
		if !im.ExportName.Valid {
			continue // uploaded after the names were assigned
		}
		rating := 0
		if im.Rating.Valid {
			rating = int(im.Rating.Int64)
		}
		x := XMPData{Rating: rating, Tags: tags[im.ID], Uploader: im.UploaderNickname}
		captured, hasTime := images.EffectiveTime(im.ManualTime, im.ExifTime, im.TimeOffsetSeconds)
		if im.ManualTime.Valid || im.TimeOffsetSeconds.Valid {
			x.Captured = captured // the sidecar only overrides the EXIF time when it was corrected
		}
		if im.GpsLat.Valid && im.GpsLon.Valid {
			x.HasGPS, x.Lat, x.Lon = true, im.GpsLat.Float64, im.GpsLon.Float64
		}
		created, _ := database.ParseTime(im.CreatedAt)
		f := File{
			ImageID: im.ID, Name: im.ExportName.String, DateDir: DateDir(captured, hasTime),
			Size: im.SizeBytes, SHA256: im.Sha256, Modified: created,
		}
		if !images.IsVideo(im.MimeType) {
			f.XMP = BuildXMP(x)
		}
		e.Files = append(e.Files, f)
	}
	return e, nil
}

func (s *Service) tags(ctx context.Context, imgs []sqlc.Image) (map[string][]string, error) {
	ids := make([]string, len(imgs))
	for i, im := range imgs {
		ids[i] = im.ID
	}
	tags := map[string][]string{}
	for part := range slices.Chunk(ids, 500) {
		rows, err := s.db.Q.ListTagsForImages(ctx, part)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			tags[r.ImageID] = append(tags[r.ImageID], r.Name)
		}
	}
	return tags, nil
}

// EnsureNames gives every image of the folder that has no export name yet one,
// in upload order. A name is never changed once assigned and stays reserved
// while the image exists (also in the trash), so the same photo always gets the
// same file name, whatever is selected.
func (s *Service) EnsureNames(ctx context.Context, folderID string) error {
	n, err := s.db.Q.CountImagesMissingExportName(ctx, folderID)
	if err != nil || n == 0 {
		return err
	}
	return s.db.InTx(ctx, func(q *sqlc.Queries) error {
		names, err := q.ListFolderExportNames(ctx, folderID)
		if err != nil {
			return err
		}
		used := make(map[string]bool, len(names))
		for _, n := range names {
			used[domain.NameKey(n.String)] = true
		}
		missing, err := q.ListImagesMissingExportName(ctx, folderID)
		if err != nil {
			return err
		}
		for _, m := range missing {
			name := domain.UniqueName(m.OriginalFilename, used)
			if err := q.SetImageExportName(ctx, sqlc.SetImageExportNameParams{
				ExportName: sql.NullString{String: name, Valid: true}, ID: m.ID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ZIP layout. Originals are stored as they are (photos are already
// compressed), sidecars are deflated. Every entry gets an extended timestamp
// and a data descriptor, as archive/zip writes them; ZipSize mirrors that.

// newDeflater returns the compressor for sidecars, used both for ZipSize and
// in the ZIP itself. Entries are written one after another, so it reuses a
// single flate writer: they are large.
func newDeflater() zip.Compressor {
	var fw *flate.Writer
	return func(w io.Writer) (io.WriteCloser, error) {
		if fw == nil {
			var err error
			fw, err = flate.NewWriter(w, flate.DefaultCompression)
			return fw, err
		}
		fw.Reset(w)
		return fw, nil
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

type zipEntry struct {
	name      string
	comp, raw int64 // compressed and uncompressed size
}

func (e *Export) entries() []zipEntry {
	out := make([]zipEntry, 0, 2*len(e.Files))
	deflate := newDeflater()
	for _, f := range e.Files {
		out = append(out, zipEntry{name: e.path(f, f.Name), comp: f.Size, raw: f.Size})
		if f.XMP == nil {
			continue
		}
		var c countWriter
		fw, _ := deflate(&c)
		fw.Write(f.XMP)
		fw.Close()
		out = append(out, zipEntry{name: e.path(f, f.XMPName()), comp: c.n, raw: int64(len(f.XMP))})
	}
	return out
}

// ZipSize is the exact length of the ZIP that WriteZip writes, so that it can
// be sent as Content-Length.
func (e *Export) ZipSize() int64 {
	return zipSize(e.entries())
}

func zipSize(entries []zipEntry) int64 {
	const (
		u32       = 1<<32 - 1
		u16       = 1<<16 - 1
		extTime   = 9 // extended timestamp extra field
		local     = 30
		central   = 46
		desc      = 16
		desc64    = 24
		end       = 22
		end64     = 56 + 20 // zip64 end record and locator
		zip64Head = 4       // zip64 extra field header, plus 8 per value
	)
	var off, dir int64
	zip64 := false
	for _, e := range entries {
		name := int64(len(e.name))
		d := int64(desc)
		if e.comp > u32 || e.raw > u32 {
			d = desc64
		}
		c := central + name + extTime
		n := 0
		for _, v := range []int64{e.comp, e.raw, off} {
			if v >= u32 {
				n++
			}
		}
		if n > 0 {
			c += zip64Head + 8*int64(n)
			zip64 = true
		}
		off += local + name + extTime + e.comp + d
		dir += c
	}
	size := off + dir + end
	if zip64 || len(entries) >= u16 || dir >= u32 || off >= u32 {
		size += end64
	}
	return size
}

// WriteZip streams the export as a ZIP to w. An error part way leaves w with
// a truncated archive; the caller should abort the response.
func (s *Service) WriteZip(ctx context.Context, w io.Writer, e *Export) error {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, newDeflater())
	for _, f := range e.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.writeOriginal(ctx, zw, e, f); err != nil {
			return err
		}
		if f.XMP == nil {
			continue
		}
		xw, err := zw.CreateHeader(&zip.FileHeader{Name: e.path(f, f.XMPName()), Method: zip.Deflate, Modified: e.Created})
		if err != nil {
			return err
		}
		if _, err := xw.Write(f.XMP); err != nil {
			return err
		}
	}
	return zw.Close()
}

func (s *Service) writeOriginal(ctx context.Context, zw *zip.Writer, e *Export, f File) error {
	rc, err := s.store.Get(ctx, storage.OriginalKey(e.FolderID, f.ImageID))
	if err != nil {
		return fmt.Errorf("original of %s: %w", f.ImageID, err)
	}
	defer rc.Close()
	w, err := zw.CreateHeader(&zip.FileHeader{Name: e.path(f, f.Name), Method: zip.Store, Modified: f.Modified})
	if err != nil {
		return err
	}
	// The size was announced up front: anything else would corrupt the archive.
	n, err := io.Copy(w, io.LimitReader(rc, f.Size+1))
	if err != nil {
		return fmt.Errorf("original of %s: %w", f.ImageID, err)
	}
	if n != f.Size {
		return fmt.Errorf("original of %s has %d bytes, expected %d", f.ImageID, n, f.Size)
	}
	return nil
}
