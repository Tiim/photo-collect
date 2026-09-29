package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"os"
	"path/filepath"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/storage"
)

// Handlers implements the core background jobs (derivatives, folder deletion).
type Handlers struct {
	DB        *database.DB
	Store     storage.Store
	Processor images.Processor
	Queue     *Queue
	ExportDir string
	Log       *slog.Logger
}

// Register installs the handlers on q.
func (h *Handlers) Register(q *Queue) {
	q.Handle(TypeDeriveImage, h.deriveImage)
	q.Handle(TypeAnalyzeImage, h.analyzeImage)
	q.Handle(TypeDeleteFolder, h.deleteFolder)
	q.Handle(TypeScanFolderDuplicates, h.scanFolderDuplicates)
}

func (h *Handlers) deriveImage(ctx context.Context, raw json.RawMessage) error {
	var p DerivePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	img, err := h.DB.Q.GetImage(ctx, p.ImageID) // excludes images of deleted folders
	if errors.Is(err, sql.ErrNoRows) {
		return nil // image or folder deleted meanwhile
	}
	if err != nil {
		return err
	}

	rc, err := h.Store.Get(ctx, storage.OriginalKey(img.FolderID, img.ID))
	if err != nil {
		return fmt.Errorf("read original: %w", err)
	}
	defer rc.Close()
	d, err := h.Processor.Derive(ctx, rc, img.MimeType)
	if err != nil {
		h.Log.Error("image processing failed", "image_id", img.ID, "err", err)
		return err
	}

	if err := h.Store.Put(ctx, storage.PreviewKey(img.FolderID, img.ID), bytes.NewReader(d.Preview), int64(len(d.Preview)), "image/jpeg"); err != nil {
		return fmt.Errorf("store preview: %w", err)
	}
	if err := h.DB.Q.MarkPreviewReady(ctx, img.ID); err != nil {
		return err
	}
	if err := h.Store.Put(ctx, storage.ThumbnailKey(img.FolderID, img.ID), bytes.NewReader(d.Thumbnail), int64(len(d.Thumbnail)), "image/jpeg"); err != nil {
		return fmt.Errorf("store thumbnail: %w", err)
	}
	if err := h.DB.Q.MarkThumbnailReady(ctx, img.ID); err != nil {
		return err
	}

	if !d.PHashOK {
		return nil
	}
	if err := h.DB.Q.SetImagePHash(ctx, sqlc.SetImagePHashParams{
		Phash: sql.NullInt64{Int64: int64(d.PHash), Valid: true}, ID: img.ID,
	}); err != nil {
		return err
	}
	// Debounce the folder's duplicate scan again now that this image's hash
	// is actually available, in case the scan already fired earlier.
	return h.Queue.ScheduleFolderScan(ctx, h.DB.Q, img.FolderID)
}

// scanFolderDuplicates compares every image's perceptual hash against every
// other in the folder and flags near-duplicate pairs for manual review.
// Exact byte-identical duplicates never reach this: they are skipped at
// upload time (see uploads.Service.Ingest), so every flagged pair here is a
// perceptual (not exact) match.
func (h *Handlers) scanFolderDuplicates(ctx context.Context, raw json.RawMessage) error {
	var p ScanFolderDuplicatesPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	rows, err := h.DB.Q.ListAllImageHashesInFolder(ctx, p.FolderID)
	if err != nil {
		return err
	}
	return h.DB.InTx(ctx, func(q *sqlc.Queries) error {
		for i := 0; i < len(rows); i++ {
			for j := i + 1; j < len(rows); j++ {
				dist := bits.OnesCount64(uint64(rows[i].Phash.Int64) ^ uint64(rows[j].Phash.Int64))
				if dist > DuplicateHashThreshold {
					continue
				}
				a, b := rows[i].ID, rows[j].ID
				if a > b {
					a, b = b, a
				}
				if err := q.InsertImageDuplicate(ctx, sqlc.InsertImageDuplicateParams{
					ID: domain.NewID(), FolderID: p.FolderID, ImageIDA: a, ImageIDB: b,
					Distance: int64(dist),
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// analyzeImage looks for a clock-calibration QR code in the image. A hit marks
// the image as a calibration shot and recomputes the clock offsets of every
// image from the same device, so the order in which photos are uploaded does
// not matter.
func (h *Handlers) analyzeImage(ctx context.Context, raw json.RawMessage) error {
	var p AnalyzePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	img, err := h.DB.Q.GetImage(ctx, p.ImageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if img.QrScanned != 0 {
		return nil
	}

	rc, err := h.Store.Get(ctx, storage.OriginalKey(img.FolderID, img.ID))
	if err != nil {
		return fmt.Errorf("read original: %w", err)
	}
	defer rc.Close()
	clock, err := h.Processor.ScanClock(ctx, rc, img.MimeType)
	if err != nil {
		h.Log.Error("clock scan failed", "image_id", img.ID, "err", err)
		return err
	}

	return h.DB.InTx(ctx, func(q *sqlc.Queries) error {
		if clock != nil {
			if err := q.SetImageCalibration(ctx, sqlc.SetImageCalibrationParams{
				CalibRefTime: sql.NullString{String: clock.Wall.Format(images.WallTimeLayout), Valid: true},
				ID:           img.ID,
			}); err != nil {
				return err
			}
			tag, err := q.UpsertTag(ctx, domain.CalibrationTag)
			if err != nil {
				return err
			}
			if err := q.AddImageTag(ctx, sqlc.AddImageTagParams{ImageID: img.ID, TagID: tag.ID}); err != nil {
				return err
			}
			if img.DeviceKey.Valid && img.ExifTime.Valid {
				if err := q.RecomputeDeviceOffsets(ctx, sqlc.RecomputeDeviceOffsetsParams{
					FolderID: img.FolderID, DeviceKey: img.DeviceKey,
				}); err != nil {
					return err
				}
			}
			h.Log.Info("clock calibration found", "image_id", img.ID, "folder_id", img.FolderID)
		}
		return q.MarkImageScanned(ctx, img.ID)
	})
}

// deleteFolder removes every stored object and export file of a
// (soft-deleted) folder and then its database rows. Retries are safe.
func (h *Handlers) deleteFolder(ctx context.Context, raw json.RawMessage) error {
	var p DeleteFolderPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	f, err := h.DB.Q.GetFolderIncludingDeleted(ctx, p.FolderID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !f.DeletedAt.Valid {
		return fmt.Errorf("folder %s is not marked deleted", f.ID)
	}

	keys, err := h.Store.List(ctx, storage.FolderPrefix(f.ID))
	if err != nil {
		return fmt.Errorf("list objects: %w", err)
	}
	var failed int
	for _, k := range keys {
		if err := h.Store.Delete(ctx, k); err != nil {
			failed++
			h.Log.Error("delete object", "key", k, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d objects could not be deleted", failed, len(keys))
	}

	exports, err := h.DB.Q.ListFolderExportKeys(ctx, f.ID)
	if err != nil {
		return err
	}
	for _, e := range exports {
		if e.FileKey.Valid {
			if err := os.Remove(filepath.Join(h.ExportDir, filepath.Base(e.FileKey.String))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}

	if err := h.DB.Q.HardDeleteFolder(ctx, f.ID); err != nil {
		return err
	}
	h.Log.Info("folder deleted", "folder_id", f.ID, "objects", len(keys))
	return nil
}
