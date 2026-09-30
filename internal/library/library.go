// Package library holds the operations that move photos in and out of a
// folder's trash. Trashing is a soft delete; purging removes the row and
// queues the removal of the stored objects in the same transaction, so no
// path leaves orphaned objects behind.
package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/jobs"
)

// MaxBatch is the largest number of images one call accepts.
const MaxBatch = 500

var (
	ErrNothingSelected = errors.New("no images selected")
	ErrTooMany         = fmt.Errorf("too many images selected (at most %d at a time)", MaxBatch)
)

type Service struct {
	db    *database.DB
	queue *jobs.Queue
}

func New(db *database.DB, queue *jobs.Queue) *Service { return &Service{db: db, queue: queue} }

func check(ids []string) error {
	switch {
	case len(ids) == 0:
		return ErrNothingSelected
	case len(ids) > MaxBatch:
		return ErrTooMany
	}
	return nil
}

// TrashIn moves the given images of folderID to the trash using q (which may
// be a transaction). IDs that are not in the folder or already trashed are
// ignored; the number of images actually trashed is returned.
func TrashIn(ctx context.Context, q *sqlc.Queries, folderID string, ids []string, userID string) (int64, error) {
	if err := check(ids); err != nil {
		return 0, err
	}
	return q.TrashImages(ctx, sqlc.TrashImagesParams{
		DeletedAt: sql.NullString{String: database.Time(time.Now()), Valid: true},
		DeletedBy: sql.NullString{String: userID, Valid: userID != ""},
		FolderID:  folderID,
		ImageIds:  ids,
	})
}

// Trash moves images to the trash. See TrashIn.
func (s *Service) Trash(ctx context.Context, folderID string, ids []string, userID string) (int64, error) {
	if err := check(ids); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.InTx(ctx, func(q *sqlc.Queries) (err error) {
		n, err = TrashIn(ctx, q, folderID, ids, userID)
		return err
	})
	return n, err
}

// TrashAll is Trash for an arbitrarily long list (all photos matching a
// filter): the images are moved in one transaction, in chunks of MaxBatch.
func (s *Service) TrashAll(ctx context.Context, folderID string, ids []string, userID string) (int64, error) {
	if len(ids) == 0 {
		return 0, ErrNothingSelected
	}
	var n int64
	err := s.db.InTx(ctx, func(q *sqlc.Queries) error {
		n = 0
		for start := 0; start < len(ids); start += MaxBatch {
			c, err := TrashIn(ctx, q, folderID, ids[start:min(start+MaxBatch, len(ids))], userID)
			if err != nil {
				return err
			}
			n += c
		}
		return nil
	})
	return n, err
}

// Restore takes images out of the trash. IDs that are not trashed are ignored.
func (s *Service) Restore(ctx context.Context, folderID string, ids []string) (int64, error) {
	if err := check(ids); err != nil {
		return 0, err
	}
	return s.db.Q.RestoreImages(ctx, sqlc.RestoreImagesParams{FolderID: folderID, ImageIds: ids})
}

// Purge permanently deletes trashed images: the row (cascading to tags,
// duplicate pairs and export links) and, through a queued job, the stored
// original, preview and thumbnail. Images that are not in the trash are
// ignored, so a photo can never be purged without going through the trash.
func (s *Service) Purge(ctx context.Context, folderID string, ids []string) (int64, error) {
	if err := check(ids); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.InTx(ctx, func(q *sqlc.Queries) error {
		imgs, err := q.ListTrashedImagesByIDs(ctx, sqlc.ListTrashedImagesByIDsParams{FolderID: folderID, ImageIds: ids})
		if err != nil {
			return err
		}
		n = 0
		for _, img := range imgs {
			if _, err := q.HardDeleteImage(ctx, img.ID); err != nil {
				return err
			}
			if err := s.queue.Enqueue(ctx, q, jobs.TypeDeleteImageObjects, jobs.DeleteImageObjectsPayload{
				FolderID: folderID, ImageID: img.ID,
			}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
