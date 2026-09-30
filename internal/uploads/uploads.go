// Package uploads ingests uploaded image files: it validates content, stores
// the untouched original and records the image, its tags and a derivative job.
package uploads

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/storage"
)

// errDuplicateRace signals that InsertImage lost a race against a concurrent
// upload of the same file (same folder, same SHA-256): the caller should
// return the winner's image rather than propagate this as a failure.
var errDuplicateRace = errors.New("uploads: concurrent duplicate upload")

var (
	ErrTooLarge   = errors.New("file is too large")
	ErrFolderFull = errors.New("this folder has reached its maximum number of images")
	ErrFolderGone = errors.New("folder no longer exists")
	ErrEmptyFile  = errors.New("file is empty")
)

type Limits struct {
	MaxFileSize        int64
	MaxImagesPerFolder int
	MaxPixels          int64
}

type Service struct {
	db     *database.DB
	store  storage.Store
	queue  *jobs.Queue
	limits Limits
	log    *slog.Logger
}

func New(db *database.DB, store storage.Store, queue *jobs.Queue, limits Limits, log *slog.Logger) *Service {
	return &Service{db: db, store: store, queue: queue, limits: limits, log: log}
}

// Ingest stores one uploaded file. r is streamed to a temporary file (never
// held in memory); rejected files leave no storage objects behind.
func (s *Service) Ingest(ctx context.Context, folderID, nickname, filename string, r io.Reader) (*sqlc.Image, error) {
	if err := s.checkCapacity(ctx, s.db.Q, folderID); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp("", "upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, s.limits.MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if n > s.limits.MaxFileSize {
		return nil, ErrTooLarge
	}
	if n == 0 {
		return nil, ErrEmptyFile
	}

	info, err := images.Inspect(tmp, s.limits.MaxPixels)
	if err != nil {
		return nil, err
	}
	meta := images.ReadMeta(tmp)
	deviceKey := meta.DeviceKey(nickname)
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	sha256Hex := hex.EncodeToString(h.Sum(nil))

	// Fast path: an exact byte-identical copy already exists in this folder.
	// Skip the upload entirely (no storage write, no new row) and hand back
	// the existing image, so a guest who double-uploads the same photo
	// notices nothing.
	if existing, err := s.db.Q.GetImageBySha256InFolder(ctx, sqlc.GetImageBySha256InFolderParams{
		FolderID: folderID, Sha256: sha256Hex,
	}); err == nil {
		return &existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	imageID := domain.NewID()
	key := storage.OriginalKey(folderID, imageID)
	if err := s.store.Put(ctx, key, tmp, n, info.MIME); err != nil {
		return nil, fmt.Errorf("store original: %w", err)
	}

	var img sqlc.Image
	err = s.db.InTx(ctx, func(q *sqlc.Queries) error {
		if err := s.checkCapacity(ctx, q, folderID); err != nil {
			return err
		}
		var err error
		img, err = q.InsertImage(ctx, sqlc.InsertImageParams{
			ID:               imageID,
			FolderID:         folderID,
			OriginalFilename: domain.SafeFilename(filename),
			MimeType:         info.MIME,
			SizeBytes:        n,
			Width:            int64(info.Width),
			Height:           int64(info.Height),
			Sha256:           sha256Hex,
			UploaderNickname: nickname,
			DeviceKey:        sql.NullString{String: deviceKey, Valid: deviceKey != ""},
			ExifTime:         sql.NullString{String: meta.TakenAt.Format(images.WallTimeLayout), Valid: !meta.TakenAt.IsZero()},
			GpsLat:           sql.NullFloat64{Float64: meta.Lat, Valid: meta.HasGPS},
			GpsLon:           sql.NullFloat64{Float64: meta.Lon, Valid: meta.HasGPS},
			GpsAttemptedAt:   sql.NullString{String: database.Time(time.Now()), Valid: true},
		})
		if err != nil {
			var sqliteErr *sqlite.Error
			if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlitelib.SQLITE_CONSTRAINT_UNIQUE {
				// Lost a race against a concurrent upload of the same file.
				return errDuplicateRace
			}
			return err
		}
		if err := s.applyInitialTags(ctx, q, folderID, imageID, nickname); err != nil {
			return err
		}
		if err := s.queue.Enqueue(ctx, q, jobs.TypeDeriveImage, jobs.DerivePayload{ImageID: imageID}); err != nil {
			return err
		}
		if err := s.queue.Enqueue(ctx, q, jobs.TypeAnalyzeImage, jobs.AnalyzePayload{ImageID: imageID}); err != nil {
			return err
		}
		return s.queue.ScheduleFolderScan(ctx, q, folderID)
	})
	if errors.Is(err, errDuplicateRace) {
		if delErr := s.store.Delete(context.WithoutCancel(ctx), key); delErr != nil {
			s.log.Error("cleanup orphaned original", "key", key, "err", delErr)
		}
		existing, err := s.db.Q.GetImageBySha256InFolder(ctx, sqlc.GetImageBySha256InFolderParams{
			FolderID: folderID, Sha256: sha256Hex,
		})
		if err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if err != nil {
		// Don't leave an orphaned object behind.
		if delErr := s.store.Delete(context.WithoutCancel(ctx), key); delErr != nil {
			s.log.Error("cleanup orphaned original", "key", key, "err", delErr)
		}
		return nil, err
	}
	return &img, nil
}

func (s *Service) checkCapacity(ctx context.Context, q *sqlc.Queries, folderID string) error {
	if _, err := q.GetFolder(ctx, folderID); errors.Is(err, sql.ErrNoRows) {
		return ErrFolderGone
	} else if err != nil {
		return err
	}
	count, err := q.CountAllImagesInFolder(ctx, folderID)
	if err != nil {
		return err
	}
	if count >= int64(s.limits.MaxImagesPerFolder) {
		return ErrFolderFull
	}
	return nil
}

func (s *Service) applyInitialTags(ctx context.Context, q *sqlc.Queries, folderID, imageID, nickname string) error {
	std, err := q.ListFolderStandardTags(ctx, folderID)
	if err != nil {
		return err
	}
	for _, t := range std {
		if err := q.AddImageTag(ctx, sqlc.AddImageTagParams{ImageID: imageID, TagID: t.ID}); err != nil {
			return err
		}
	}
	up, err := q.UpsertTag(ctx, domain.UploaderTag(nickname))
	if err != nil {
		return err
	}
	return q.AddImageTag(ctx, sqlc.AddImageTagParams{ImageID: imageID, TagID: up.ID})
}
