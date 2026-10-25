package http

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/library"
)

// ---- view models ----

type DuplicateImage struct {
	ID       string
	Filename string
}

// DuplicateCluster is a group of images linked by pending near-duplicate
// pairs: if A~B and A~C are flagged, A, B and C form one cluster. ID is the
// oldest pair in the cluster and addresses it in the action URLs.
type DuplicateCluster struct {
	ID      string
	Images  []DuplicateImage
	PairIDs []string
}

type DuplicatesView struct {
	FolderID string
	Clusters []DuplicateCluster
	Open     bool // expanded; set after the user acted on a cluster
}

// clusterDuplicates groups pending pairs (oldest first) into connected
// components. Clusters and the images in them keep the order in which they
// first appear in rows.
func clusterDuplicates(rows []sqlc.ListPendingDuplicatesRow) []DuplicateCluster {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	var order []string // images in first-seen order
	names := map[string]string{}
	add := func(id, name string) {
		if _, ok := parent[id]; !ok {
			parent[id] = id
			names[id] = name
			order = append(order, id)
		}
	}
	for _, r := range rows {
		add(r.ImageIDA, r.AFilename)
		add(r.ImageIDB, r.BFilename)
		if a, b := find(r.ImageIDA), find(r.ImageIDB); a != b {
			parent[b] = a
		}
	}

	idx := map[string]int{} // root -> index in clusters
	var clusters []DuplicateCluster
	for _, r := range rows {
		root := find(r.ImageIDA)
		i, ok := idx[root]
		if !ok {
			i = len(clusters)
			idx[root] = i
			clusters = append(clusters, DuplicateCluster{ID: r.ID})
		}
		clusters[i].PairIDs = append(clusters[i].PairIDs, r.ID)
	}
	for _, id := range order {
		c := &clusters[idx[find(id)]]
		c.Images = append(c.Images, DuplicateImage{ID: id, Filename: names[id]})
	}
	return clusters
}

func (s *Server) duplicatesView(ctx context.Context, folderID string) (DuplicatesView, error) {
	rows, err := s.db.Q.ListPendingDuplicates(ctx, folderID)
	if err != nil {
		return DuplicatesView{}, err
	}
	return DuplicatesView{FolderID: folderID, Clusters: clusterDuplicates(rows)}, nil
}

// ---- handlers ----

// duplicateCluster loads the cluster containing the pending pair named in the
// path, or writes a 404 if the pair doesn't exist, was already
// resolved/dismissed, or involves a trashed image.
func (s *Server) duplicateCluster(w http.ResponseWriter, r *http.Request) (string, DuplicateCluster, bool) {
	d, err := s.db.Q.GetImageDuplicate(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOr(w, r, err)
		return "", DuplicateCluster{}, false
	}
	if d.Status == "pending" {
		v, err := s.duplicatesView(r.Context(), d.FolderID)
		if err != nil {
			s.serverError(w, r, err)
			return "", DuplicateCluster{}, false
		}
		for _, c := range v.Clusters {
			if slices.Contains(c.PairIDs, d.ID) {
				return d.FolderID, c, true
			}
		}
	}
	http.NotFound(w, r)
	return "", DuplicateCluster{}, false
}

// clusterImage returns the image ID from the form field if it belongs to the
// cluster, or writes a 400.
func (s *Server) clusterImage(w http.ResponseWriter, r *http.Request, c DuplicateCluster, field string) (string, bool) {
	id := r.PostFormValue(field)
	for _, img := range c.Images {
		if img.ID == id {
			return id, true
		}
	}
	s.fail(w, r, http.StatusBadRequest, "err.keep_pair")
	return "", false
}

func (s *Server) duplicatesResponse(w http.ResponseWriter, r *http.Request, folderID string) {
	v, err := s.duplicatesView(r.Context(), folderID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v.Open = true
	s.fragment(w, r, "duplicates_panel", v)
}

// dismissPairs marks the given pending pairs as "not a duplicate".
func (s *Server) dismissPairs(ctx context.Context, ids []string) error {
	now := sql.NullString{String: database.Time(time.Now()), Valid: true}
	return s.db.InTx(ctx, func(q *sqlc.Queries) error {
		for _, id := range ids {
			if _, err := q.DismissImageDuplicate(ctx, sqlc.DismissImageDuplicateParams{ResolvedAt: now, ID: id}); err != nil {
				return err
			}
		}
		return nil
	})
}

// duplicateDismiss marks the whole cluster as "not duplicates".
func (s *Server) duplicateDismiss(w http.ResponseWriter, r *http.Request) {
	folderID, c, ok := s.duplicateCluster(w, r)
	if !ok {
		return
	}
	if err := s.dismissPairs(r.Context(), c.PairIDs); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.duplicatesResponse(w, r, folderID)
}

// duplicateExclude takes one image out of its cluster by dismissing every
// pending pair it is part of. The remaining images stay flagged (and may
// split into smaller clusters if that image was what linked them).
func (s *Server) duplicateExclude(w http.ResponseWriter, r *http.Request) {
	folderID, c, ok := s.duplicateCluster(w, r)
	if !ok {
		return
	}
	img, ok := s.clusterImage(w, r, c, "image")
	if !ok {
		return
	}
	rows, err := s.db.Q.ListPendingDuplicates(r.Context(), folderID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var ids []string
	for _, row := range rows {
		if row.ImageIDA == img || row.ImageIDB == img {
			ids = append(ids, row.ID)
		}
	}
	if err := s.dismissPairs(r.Context(), ids); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.duplicatesResponse(w, r, folderID)
}

// duplicateResolve keeps one image of a flagged cluster and moves all the
// others to the trash: their tags are unioned onto the kept image and its
// rating is filled in (with the highest rating among them) only if it had
// none. The trashed images can be restored or purged from the trash view
// like any other.
func (s *Server) duplicateResolve(w http.ResponseWriter, r *http.Request) {
	folderID, c, ok := s.duplicateCluster(w, r)
	if !ok {
		return
	}
	keep, ok := s.clusterImage(w, r, c, "keep")
	if !ok {
		return
	}
	var del []string
	for _, img := range c.Images {
		if img.ID != keep {
			del = append(del, img.ID)
		}
	}

	err := s.db.InTx(r.Context(), func(q *sqlc.Queries) error {
		keepImg, err := q.GetImageUnchecked(r.Context(), keep)
		if err != nil {
			return err
		}
		rating := keepImg.Rating
		now := sql.NullString{String: database.Time(time.Now()), Valid: true}
		for _, id := range del {
			tags, err := q.ListImageTags(r.Context(), id)
			if err != nil {
				return err
			}
			for _, t := range tags {
				if err := q.AddImageTag(r.Context(), sqlc.AddImageTagParams{ImageID: keep, TagID: t.ID}); err != nil {
					return err
				}
			}
			if !keepImg.Rating.Valid {
				img, err := q.GetImageUnchecked(r.Context(), id)
				if err != nil {
					return err
				}
				if img.Rating.Valid && (!rating.Valid || img.Rating.Int64 > rating.Int64) {
					rating = img.Rating
				}
			}
			if err := q.ResolveDuplicatesForImage(r.Context(), sqlc.ResolveDuplicatesForImageParams{
				ResolvedAt: now, ImageIDA: id, ImageIDB: id,
			}); err != nil {
				return err
			}
		}
		if rating != keepImg.Rating {
			if _, err := q.SetImageRating(r.Context(), sqlc.SetImageRatingParams{Rating: rating, ID: keep}); err != nil {
				return err
			}
		}
		_, err = library.TrashIn(r.Context(), q, folderID, del, sessionFrom(r.Context()).UserID)
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("duplicate resolved", "kept", keep, "trashed", del, "user_id", sessionFrom(r.Context()).UserID)
	s.duplicatesResponse(w, r, folderID)
}
