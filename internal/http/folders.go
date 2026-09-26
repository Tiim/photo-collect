package http

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/jobs"
)

const pageSize = 60

// ---- view models ----

type TileView struct {
	sqlc.Image
	RatingValue int
}

func tile(img sqlc.Image) TileView { return TileView{Image: img, RatingValue: ratingOf(img)} }

func ratingOf(img sqlc.Image) int {
	if img.Rating.Valid {
		return int(img.Rating.Int64)
	}
	return 0
}

type GridView struct {
	FolderID   string
	Images     []TileView
	NextBefore int64 // 0 when there are no more images
}

type LinkView struct {
	FolderID    string
	Exists      bool
	URL         string
	ExpiresAt   string
	Expired     bool
	DefaultDays int
}

type ExportRow struct {
	sqlc.Export
	Ready bool
}

type ExportsView struct {
	FolderID string
	Exports  []ExportRow
	Active   bool // something is still being built: keep polling
}

type StdTagsView struct {
	FolderID string
	Tags     []sqlc.Tag
}

type FolderPage struct {
	Folder  sqlc.Folder
	Tags    StdTagsView
	Link    LinkView
	Exports ExportsView
	Grid    GridView
	Count   int64
}

// ---- handlers ----

func (s *Server) foldersList(w http.ResponseWriter, r *http.Request) {
	folders, err := s.db.Q.ListFolders(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "folders", "Folders", folders)
}

func (s *Server) folderCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.Join(strings.Fields(r.PostFormValue("name")), " ")
	if name == "" || len([]rune(name)) > 100 {
		http.Error(w, "Folder name must be between 1 and 100 characters", http.StatusBadRequest)
		return
	}
	sess := sessionFrom(r.Context())
	f, err := s.db.Q.CreateFolder(r.Context(), sqlc.CreateFolderParams{
		ID: domain.NewID(), Name: name, CreatedBy: sql.NullString{String: sess.UserID, Valid: true},
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("folder created", "folder_id", f.ID, "user_id", sess.UserID)
	redirect(w, r, "/folders/"+f.ID)
}

// folder loads the folder named in the path or writes a 404.
func (s *Server) folder(w http.ResponseWriter, r *http.Request) (sqlc.Folder, bool) {
	f, err := s.db.Q.GetFolder(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOr(w, r, err)
		return f, false
	}
	return f, true
}

func (s *Server) folderShow(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	p := FolderPage{Folder: f}
	var err error
	if p.Tags, err = s.stdTagsView(ctx, f.ID); err == nil {
		if p.Link, err = s.linkView(ctx, f.ID); err == nil {
			if p.Exports, err = s.exportsView(ctx, f.ID); err == nil {
				if p.Grid, err = s.gridView(ctx, f.ID, math.MaxInt64); err == nil {
					p.Count, err = s.db.Q.CountImagesInFolder(ctx, f.ID)
				}
			}
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "folder", f.Name, p)
}

func (s *Server) folderImages(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || before <= 0 {
		before = math.MaxInt64
	}
	g, err := s.gridView(r.Context(), f.ID, before)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "image_grid", g)
}

func (s *Server) gridView(ctx context.Context, folderID string, before int64) (GridView, error) {
	imgs, err := s.db.Q.ListImagesInFolder(ctx, sqlc.ListImagesInFolderParams{
		FolderID: folderID, BeforeSeq: before, PageSize: pageSize + 1,
	})
	if err != nil {
		return GridView{}, err
	}
	g := GridView{FolderID: folderID}
	if len(imgs) > pageSize {
		imgs = imgs[:pageSize]
		g.NextBefore = imgs[len(imgs)-1].Seq
	}
	for _, im := range imgs {
		g.Images = append(g.Images, tile(im))
	}
	return g, nil
}

func (s *Server) folderDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := sessionFrom(r.Context())
	var deleted bool
	err := s.db.InTx(r.Context(), func(q *sqlc.Queries) error {
		n, err := q.SoftDeleteFolder(r.Context(), sqlc.SoftDeleteFolderParams{
			DeletedAt: sql.NullString{String: database.Time(time.Now()), Valid: true}, ID: id,
		})
		if err != nil || n == 0 {
			return err
		}
		deleted = true
		if err := q.DeleteUploadLink(r.Context(), id); err != nil {
			return err
		}
		return s.queue.Enqueue(r.Context(), q, jobs.TypeDeleteFolder, jobs.DeleteFolderPayload{FolderID: id})
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !deleted {
		http.NotFound(w, r)
		return
	}
	s.log.Info("folder deleted (cleanup queued)", "folder_id", id, "user_id", sess.UserID)
	redirect(w, r, "/folders")
}

// ---- standard tags ----

func (s *Server) stdTagsView(ctx context.Context, folderID string) (StdTagsView, error) {
	tags, err := s.db.Q.ListFolderStandardTags(ctx, folderID)
	return StdTagsView{FolderID: folderID, Tags: tags}, err
}

func (s *Server) standardTagAdd(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	name, err := domain.NormalizeTag(r.PostFormValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err = s.db.InTx(r.Context(), func(q *sqlc.Queries) error {
		t, err := q.UpsertTag(r.Context(), name)
		if err != nil {
			return err
		}
		return q.AddFolderStandardTag(r.Context(), sqlc.AddFolderStandardTagParams{FolderID: f.ID, TagID: t.ID})
	})
	s.stdTagsResponse(w, r, f.ID, err)
}

func (s *Server) standardTagRemove(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	tagID, err := strconv.ParseInt(r.PathValue("tag"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.db.Q.RemoveFolderStandardTag(r.Context(), sqlc.RemoveFolderStandardTagParams{FolderID: f.ID, TagID: tagID})
	s.stdTagsResponse(w, r, f.ID, err)
}

func (s *Server) stdTagsResponse(w http.ResponseWriter, r *http.Request, folderID string, err error) {
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v, err := s.stdTagsView(r.Context(), folderID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "standard_tags", v)
}

// ---- upload link ----

func (s *Server) linkView(ctx context.Context, folderID string) (LinkView, error) {
	v := LinkView{FolderID: folderID, DefaultDays: int(s.cfg.UploadLinkDuration.Hours() / 24)}
	if v.DefaultDays < 1 {
		v.DefaultDays = 1
	}
	l, err := s.db.Q.GetUploadLinkByFolder(ctx, folderID)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	v.Exists = true
	v.URL = s.publicURL("upload", l.Token)
	v.ExpiresAt = l.ExpiresAt
	v.Expired = l.ExpiresAt <= database.Time(time.Now())
	return v, nil
}

func (s *Server) linkDuration(r *http.Request) time.Duration {
	if days, err := strconv.Atoi(r.PostFormValue("days")); err == nil && days >= 1 && days <= 365 {
		return time.Duration(days) * 24 * time.Hour
	}
	return s.cfg.UploadLinkDuration
}

func (s *Server) linkResponse(w http.ResponseWriter, r *http.Request, folderID string, err error) {
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v, err := s.linkView(r.Context(), folderID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "upload_link", v)
}

// uploadLinkCreate creates the folder's link, or regenerates it with a fresh
// token (invalidating the previous one immediately).
func (s *Server) uploadLinkCreate(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	_, err := s.db.Q.UpsertUploadLink(r.Context(), sqlc.UpsertUploadLinkParams{
		FolderID: f.ID, Token: domain.NewToken(), ExpiresAt: database.Time(time.Now().Add(s.linkDuration(r))),
	})
	if err == nil {
		s.log.Info("upload link created", "folder_id", f.ID, "user_id", sessionFrom(r.Context()).UserID)
	}
	s.linkResponse(w, r, f.ID, err)
}

// uploadLinkExtend keeps the token but sets a new expiry.
func (s *Server) uploadLinkExtend(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	n, err := s.db.Q.ExtendUploadLink(r.Context(), sqlc.ExtendUploadLinkParams{
		ExpiresAt: database.Time(time.Now().Add(s.linkDuration(r))), FolderID: f.ID,
	})
	if err == nil && n == 0 {
		http.NotFound(w, r)
		return
	}
	s.linkResponse(w, r, f.ID, err)
}

func (s *Server) uploadLinkRevoke(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	err := s.db.Q.DeleteUploadLink(r.Context(), f.ID)
	if err == nil {
		s.log.Info("upload link revoked", "folder_id", f.ID, "user_id", sessionFrom(r.Context()).UserID)
	}
	s.linkResponse(w, r, f.ID, err)
}
