package http

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/library"
)

// ---- view models ----

type DuplicatePair struct {
	ID        string
	ImageIDA  string
	ImageIDB  string
	AFilename string
	BFilename string
	Distance  int64
}

type DuplicatesView struct {
	FolderID string
	Pending  []DuplicatePair
}

func (s *Server) duplicatesView(ctx context.Context, folderID string) (DuplicatesView, error) {
	rows, err := s.db.Q.ListPendingDuplicates(ctx, folderID)
	if err != nil {
		return DuplicatesView{}, err
	}
	v := DuplicatesView{FolderID: folderID}
	for _, row := range rows {
		v.Pending = append(v.Pending, DuplicatePair{
			ID: row.ID, ImageIDA: row.ImageIDA, ImageIDB: row.ImageIDB,
			AFilename: row.AFilename, BFilename: row.BFilename, Distance: row.Distance,
		})
	}
	return v, nil
}

// ---- handlers ----

// duplicate loads the pending image_duplicates row named in the path, or
// writes a 404 if it doesn't exist or was already resolved/dismissed.
func (s *Server) duplicate(w http.ResponseWriter, r *http.Request) (sqlc.ImageDuplicate, bool) {
	d, err := s.db.Q.GetImageDuplicate(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOr(w, r, err)
		return d, false
	}
	if d.Status != "pending" {
		http.NotFound(w, r)
		return d, false
	}
	return d, true
}

func (s *Server) duplicatesResponse(w http.ResponseWriter, r *http.Request, folderID string) {
	v, err := s.duplicatesView(r.Context(), folderID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "duplicates_panel", v)
}

func (s *Server) duplicateDismiss(w http.ResponseWriter, r *http.Request) {
	d, ok := s.duplicate(w, r)
	if !ok {
		return
	}
	if _, err := s.db.Q.DismissImageDuplicate(r.Context(), sqlc.DismissImageDuplicateParams{
		ResolvedAt: sql.NullString{String: database.Time(time.Now()), Valid: true}, ID: d.ID,
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.duplicatesResponse(w, r, d.FolderID)
}

// duplicateResolve keeps one image of a flagged pair and moves the other to
// the trash: tags are unioned onto the kept image and its rating is filled in
// only if it had none. The trashed image can be restored or purged from the
// trash view like any other.
func (s *Server) duplicateResolve(w http.ResponseWriter, r *http.Request) {
	d, ok := s.duplicate(w, r)
	if !ok {
		return
	}
	keep := r.PostFormValue("keep")
	var del string
	switch keep {
	case d.ImageIDA:
		del = d.ImageIDB
	case d.ImageIDB:
		del = d.ImageIDA
	default:
		http.Error(w, "keep must be one of the pair's images", http.StatusBadRequest)
		return
	}

	delImg, err := s.db.Q.GetImageUnchecked(r.Context(), del)
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}

	err = s.db.InTx(r.Context(), func(q *sqlc.Queries) error {
		tags, err := q.ListImageTags(r.Context(), del)
		if err != nil {
			return err
		}
		for _, t := range tags {
			if err := q.AddImageTag(r.Context(), sqlc.AddImageTagParams{ImageID: keep, TagID: t.ID}); err != nil {
				return err
			}
		}
		if delImg.Rating.Valid {
			keepImg, err := q.GetImageUnchecked(r.Context(), keep)
			if err != nil {
				return err
			}
			if !keepImg.Rating.Valid {
				if _, err := q.SetImageRating(r.Context(), sqlc.SetImageRatingParams{Rating: delImg.Rating, ID: keep}); err != nil {
					return err
				}
			}
		}
		now := sql.NullString{String: database.Time(time.Now()), Valid: true}
		if err := q.ResolveDuplicatesForImage(r.Context(), sqlc.ResolveDuplicatesForImageParams{
			ResolvedAt: now, ImageIDA: del, ImageIDB: del,
		}); err != nil {
			return err
		}
		_, err = library.TrashIn(r.Context(), q, delImg.FolderID, []string{del}, sessionFrom(r.Context()).UserID)
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("duplicate resolved", "kept", keep, "trashed", del, "user_id", sessionFrom(r.Context()).UserID)
	s.duplicatesResponse(w, r, d.FolderID)
}
