// Package downloads builds ZIP exports (originals plus XMP sidecars) in the background.
package downloads

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/storage"
)

type Service struct {
	db    *database.DB
	store storage.Store
	queue *jobs.Queue
	dir   string
	ttl   time.Duration
	log   *slog.Logger
}

func New(db *database.DB, store storage.Store, queue *jobs.Queue, dir string, ttl time.Duration, log *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create export dir: %w", err)
	}
	s := &Service{db: db, store: store, queue: queue, dir: dir, ttl: ttl, log: log}
	queue.Handle(jobs.TypeBuildExport, s.build)
	return s, nil
}

// Create records an export of the given images (all images of the folder if
// imageIDs is empty) and enqueues its build.
func (s *Service) Create(ctx context.Context, folderID, userID string, imageIDs []string) (sqlc.Export, error) {
	var exp sqlc.Export
	err := s.db.InTx(ctx, func(q *sqlc.Queries) error {
		if _, err := q.GetFolder(ctx, folderID); err != nil {
			return err
		}
		var err error
		exp, err = q.CreateExport(ctx, sqlc.CreateExportParams{
			ID:        domain.NewID(),
			FolderID:  folderID,
			UserID:    sql.NullString{String: userID, Valid: userID != ""},
			ExpiresAt: database.Time(time.Now().Add(s.ttl)),
		})
		if err != nil {
			return err
		}
		if len(imageIDs) == 0 {
			err = q.AddAllFolderImagesToExport(ctx, sqlc.AddAllFolderImagesToExportParams{ExportID: exp.ID, FolderID: folderID})
		} else {
			imgs, e := q.ListImagesByIDs(ctx, sqlc.ListImagesByIDsParams{FolderID: folderID, ImageIds: imageIDs})
			if e != nil {
				return e
			}
			for _, im := range imgs {
				if err = q.AddExportImage(ctx, sqlc.AddExportImageParams{ExportID: exp.ID, ImageID: im.ID}); err != nil {
					break
				}
			}
		}
		if err != nil {
			return err
		}
		return s.queue.Enqueue(ctx, q, jobs.TypeBuildExport, jobs.ExportPayload{ExportID: exp.ID})
	})
	return exp, err
}

// FilePath returns the path of a ready export's ZIP file.
func (s *Service) FilePath(e sqlc.Export) string {
	return filepath.Join(s.dir, filepath.Base(e.FileKey.String))
}

func (s *Service) build(ctx context.Context, raw json.RawMessage) error {
	var p jobs.ExportPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	exp, err := s.db.Q.GetExport(ctx, p.ExportID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if exp.Status == "ready" {
		return nil
	}
	if err := s.db.Q.MarkExportRunning(ctx, exp.ID); err != nil {
		return err
	}
	size, err := s.writeZip(ctx, exp)
	if err != nil {
		s.log.Error("export failed", "export_id", exp.ID, "err", err)
		_ = s.db.Q.MarkExportFailed(context.WithoutCancel(ctx), sqlc.MarkExportFailedParams{
			Error: sql.NullString{String: err.Error(), Valid: true}, ID: exp.ID})
		return err
	}
	return s.db.Q.MarkExportReady(ctx, sqlc.MarkExportReadyParams{
		FileKey:   sql.NullString{String: exp.ID + ".zip", Valid: true},
		SizeBytes: sql.NullInt64{Int64: size, Valid: true},
		ID:        exp.ID,
	})
}

func (s *Service) writeZip(ctx context.Context, exp sqlc.Export) (int64, error) {
	imgs, err := s.db.Q.ListExportImages(ctx, exp.ID)
	if err != nil {
		return 0, err
	}
	tagRows, err := s.db.Q.ListTagsForExport(ctx, exp.ID)
	if err != nil {
		return 0, err
	}
	tags := map[string][]string{}
	for _, r := range tagRows {
		tags[r.ImageID] = append(tags[r.ImageID], r.Name)
	}
	folder, err := s.db.Q.GetFolder(ctx, exp.FolderID)
	if err != nil {
		return 0, err
	}
	dir := domain.SafeFilename(folder.Name) + "/"

	names := make([]string, len(imgs))
	for i, im := range imgs {
		names[i] = im.OriginalFilename
	}
	names = domain.UniqueNames(names)

	final := filepath.Join(s.dir, exp.ID+".zip")
	f, err := os.Create(final + ".tmp")
	if err != nil {
		return 0, err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(final + ".tmp")
		}
	}()
	zw := zip.NewWriter(f)

	for i, im := range imgs {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rc, err := s.store.Get(ctx, storage.OriginalKey(im.FolderID, im.ID))
		if errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("export: original missing, skipping", "image_id", im.ID)
			continue
		}
		if err != nil {
			return 0, err
		}
		created, _ := database.ParseTime(im.CreatedAt)
		// Photos are already compressed, so store them without deflating.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: dir + names[i], Method: zip.Store, Modified: created})
		if err == nil {
			_, err = io.Copy(w, rc)
		}
		rc.Close()
		if err != nil {
			return 0, err
		}

		rating := 0
		if im.Rating.Valid {
			rating = int(im.Rating.Int64)
		}
		ext := filepath.Ext(names[i])
		xw, err := zw.CreateHeader(&zip.FileHeader{Name: dir + strings.TrimSuffix(names[i], ext) + ".xmp", Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return 0, err
		}
		xmp := XMPData{Rating: rating, Tags: tags[im.ID], Uploader: im.UploaderNickname}
		if im.TimeOffsetSeconds.Valid {
			xmp.Captured, _ = images.CorrectedTime(im.ExifTime, im.TimeOffsetSeconds)
		}
		if _, err := xw.Write(BuildXMP(xmp)); err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(final+".tmp", final); err != nil {
		return 0, err
	}
	ok = true
	return st.Size(), nil
}

// RunCleanup periodically deletes expired exports until ctx is cancelled.
func (s *Service) RunCleanup(ctx context.Context) {
	s.cleanup(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.cleanup(ctx)
		}
	}
}

func (s *Service) cleanup(ctx context.Context) {
	expired, err := s.db.Q.ListExpiredExports(ctx, database.Time(time.Now()))
	if err != nil {
		s.log.Error("list expired exports", "err", err)
		return
	}
	for _, e := range expired {
		if err := os.Remove(filepath.Join(s.dir, e.ID+".zip")); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Error("remove expired export", "export_id", e.ID, "err", err)
			continue
		}
		if err := s.db.Q.DeleteExport(ctx, e.ID); err != nil {
			s.log.Error("delete export row", "export_id", e.ID, "err", err)
		}
	}
	// Remove leftovers from interrupted builds.
	if entries, err := os.ReadDir(s.dir); err == nil {
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), ".tmp") {
				if info, err := en.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
					os.Remove(filepath.Join(s.dir, en.Name()))
				}
			}
		}
	}
}
