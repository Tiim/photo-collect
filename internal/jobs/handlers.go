package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/storage"
)

// Handlers implements the core background jobs (derivatives, folder deletion).
type Handlers struct {
	DB        *database.DB
	Store     storage.Store
	Processor images.Processor
	ExportDir string
	Log       *slog.Logger
}

// Register installs the handlers on q.
func (h *Handlers) Register(q *Queue) {
	q.Handle(TypeDeriveImage, h.deriveImage)
	q.Handle(TypeDeleteFolder, h.deleteFolder)
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
	return h.DB.Q.MarkThumbnailReady(ctx, img.ID)
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
