package http

import (
	"context"
	"math"
	"net/http"
	"strconv"

	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/library"
)

// FolderCounts feeds the image and trash counters, which are re-rendered
// out-of-band after trash operations (OOB).
type FolderCounts struct {
	FolderID string
	Count    int64
	Trashed  int64
	OOB      bool
}

type TrashGridView struct {
	FolderID   string
	Images     []TileView
	NextBefore int64
}

type TrashPage struct {
	Folder sqlc.Folder
	Counts FolderCounts
	Grid   TrashGridView
}

func (s *Server) folderCounts(ctx context.Context, folderID string, oob bool) (FolderCounts, error) {
	c := FolderCounts{FolderID: folderID, OOB: oob}
	var err error
	if c.Count, err = s.db.Q.CountImagesInFolder(ctx, folderID); err != nil {
		return c, err
	}
	c.Trashed, err = s.db.Q.CountTrashedImagesInFolder(ctx, folderID)
	return c, err
}

func (s *Server) trashGridView(ctx context.Context, folderID string, before int64) (TrashGridView, error) {
	imgs, err := s.db.Q.ListTrashedImagesInFolder(ctx, sqlc.ListTrashedImagesInFolderParams{FolderID: folderID, BeforeSeq: before, PageSize: pageSize + 1})
	if err != nil {
		return TrashGridView{}, err
	}
	g := TrashGridView{FolderID: folderID}
	if len(imgs) > pageSize {
		imgs = imgs[:pageSize]
		g.NextBefore = imgs[len(imgs)-1].Seq
	}
	for _, im := range imgs {
		g.Images = append(g.Images, tile(im))
	}
	return g, nil
}

// selectedImages returns the image IDs posted by the grid, or writes a 400.
func selectedImages(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return nil, false
	}
	ids := r.PostForm["image"]
	if len(ids) == 0 {
		http.Error(w, "Select at least one image", http.StatusBadRequest)
		return nil, false
	}
	if len(ids) > library.MaxBatch {
		http.Error(w, library.ErrTooMany.Error(), http.StatusBadRequest)
		return nil, false
	}
	return ids, true
}

// imagesTrash moves the checked images, or with matching=1 every image that
// matches the filter in the query string, to the trash and returns the
// refreshed gallery. For "all matching" the client sends the count it
// showed the user (expect); if the filter matches a different number by
// now, nothing is trashed.
func (s *Server) imagesTrash(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	flt, ok := parseFilter(w, r)
	if !ok {
		return
	}
	userID := sessionFrom(r.Context()).UserID
	var n int64
	var err error
	if r.URL.Query().Get("matching") == "1" {
		if !flt.Active() {
			http.Error(w, "Choose a filter first", http.StatusBadRequest)
			return
		}
		ids, err := s.matchingIDs(r.Context(), f.ID, flt)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if len(ids) == 0 {
			http.Error(w, "No photos match the filter", http.StatusBadRequest)
			return
		}
		if expect, _ := strconv.Atoi(r.URL.Query().Get("expect")); expect != len(ids) {
			http.Error(w, "The photos matching the filter have changed. Reload the page and try again.", http.StatusConflict)
			return
		}
		n, err = s.library.TrashAll(r.Context(), f.ID, ids, userID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	} else {
		ids, ok := selectedImages(w, r)
		if !ok {
			return
		}
		if n, err = s.library.Trash(r.Context(), f.ID, ids, userID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.log.Info("images trashed", "folder_id", f.ID, "user_id", userID, "count", n, "matching", r.URL.Query().Get("matching") == "1")

	g, err := s.galleryView(r.Context(), f, flt)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	counts, err := s.folderCounts(r.Context(), f.ID, true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	g.Map.OOB = true
	s.fragment(w, r, "gallery_response", struct {
		Gallery GalleryView
		Counts  *FolderCounts
	}{g, &counts})
}

// imageTrash moves one image (from its detail view) to the trash.
func (s *Server) imageTrash(w http.ResponseWriter, r *http.Request) {
	img, ok := s.image(w, r)
	if !ok {
		return
	}
	userID := sessionFrom(r.Context()).UserID
	n, err := s.library.Trash(r.Context(), img.FolderID, []string{img.ID}, userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("images trashed", "folder_id", img.FolderID, "user_id", userID, "count", n)
	redirect(w, r, "/folders/"+img.FolderID)
}

// trashShow lists the trashed images of a folder (a fragment for the paging sentinel).
func (s *Server) trashShow(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || before <= 0 {
		before = math.MaxInt64
	}
	g, err := s.trashGridView(ctx, f.ID, before)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if isHTMX(r) && before != math.MaxInt64 {
		s.fragment(w, r, "trash_grid", g)
		return
	}
	counts, err := s.folderCounts(ctx, f.ID, false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "trash", "Trash · "+f.Name, TrashPage{Folder: f, Counts: counts, Grid: g})
}

func (s *Server) trashChange(purge bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, ok := s.folder(w, r)
		if !ok {
			return
		}
		ids, ok := selectedImages(w, r)
		if !ok {
			return
		}
		userID := sessionFrom(r.Context()).UserID
		var n int64
		var err error
		action := "images restored"
		if purge {
			action = "images purged"
			n, err = s.library.Purge(r.Context(), f.ID, ids)
		} else {
			n, err = s.library.Restore(r.Context(), f.ID, ids)
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.log.Info(action, "folder_id", f.ID, "user_id", userID, "count", n)

		g, err := s.trashGridView(r.Context(), f.ID, math.MaxInt64)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		counts, err := s.folderCounts(r.Context(), f.ID, true)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.fragment(w, r, "trash_response", struct {
			Grid   TrashGridView
			Counts FolderCounts
		}{g, counts})
	}
}
