package http

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
)

// Share links give people without an account read-only access to a
// hand-picked set of a folder's photos and videos until they expire. The
// token in the URL is the only credential, like for upload links.

// ShareGridView is one page of a share's tiles. NextURL fetches the next page
// and is empty on the last one.
type ShareGridView struct {
	Token   string // set on the public page; empty in the management view
	Images  []TileView
	NextURL string
}

// SharePage is the management page of one share.
type SharePage struct {
	Share       sqlc.Share
	Folder      sqlc.Folder
	URL         string
	Expired     bool
	Count       int64
	DefaultDays int
	CSRF        string
	Grid        ShareGridView
}

// PublicSharePage is what visitors of a share link see.
type PublicSharePage struct {
	Token     string
	Name      string
	ExpiresAt string
	Count     int64
	Grid      ShareGridView
}

// PublicImageView is one photo or video of a share, with its neighbours.
type PublicImageView struct {
	TileView
	Token      string
	ShareName  string
	Prev, Next string
}

// SharesView feeds the share links panel on the folder page.
type SharesView struct {
	FolderID string
	Shares   []sqlc.ListFolderSharesRow
}

// Expired reports whether a share row from ListFolderShares has expired.
func (v SharesView) Expired(expiresAt string) bool {
	return expiresAt <= database.Time(time.Now())
}

// ---- management (signed in) ----

func (s *Server) shareDefaultDays() int {
	return max(int(s.cfg.ShareLinkDuration.Hours()/24), 1)
}

func (s *Server) shareDuration(r *http.Request) time.Duration {
	if days, err := strconv.Atoi(r.PostFormValue("days")); err == nil && days >= 1 && days <= 365 {
		return time.Duration(days) * 24 * time.Hour
	}
	return s.cfg.ShareLinkDuration
}

// share loads the share named in the path (of a folder that still exists) or writes a 404.
func (s *Server) share(w http.ResponseWriter, r *http.Request) (sqlc.Share, bool) {
	sh, err := s.db.Q.GetShare(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOr(w, r, err)
		return sh, false
	}
	return sh, true
}

// shareAdd puts the checked images of the gallery into a share: the one named
// by the "share" field, or a new one when it is empty.
func (s *Server) shareAdd(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	ids, ok := s.selectedImages(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	userID := sessionFrom(ctx).UserID
	shareID := r.PostFormValue("share")
	var added int64
	err := s.db.InTx(ctx, func(q *sqlc.Queries) error {
		if shareID == "" {
			sh, err := q.CreateShare(ctx, sqlc.CreateShareParams{
				ID: domain.NewID(), FolderID: f.ID, Token: domain.NewToken(), Name: f.Name,
				ExpiresAt: database.Time(time.Now().Add(s.cfg.ShareLinkDuration)),
				CreatedBy: sql.NullString{String: userID, Valid: true},
			})
			if err != nil {
				return err
			}
			shareID = sh.ID
			s.log.Info("share created", "share_id", sh.ID, "folder_id", f.ID, "user_id", userID)
		} else if sh, err := q.GetShare(ctx, shareID); err != nil {
			return err
		} else if sh.FolderID != f.ID {
			return sql.ErrNoRows
		}
		var err error
		added, err = q.AddShareImages(ctx, sqlc.AddShareImagesParams{ShareID: shareID, FolderID: f.ID, ImageIds: ids})
		return err
	})
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}
	s.log.Info("images shared", "share_id", shareID, "folder_id", f.ID, "user_id", userID, "count", added)
	redirect(w, r, "/shares/"+shareID)
}

// shareShow renders the management page, or the next page of its grid for
// the paging sentinel.
func (s *Server) shareShow(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.share(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	g, err := s.shareGrid(ctx, sh, "", after)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if after > 0 {
		s.fragment(w, r, "share_grid", g)
		return
	}
	f, err := s.db.Q.GetFolder(ctx, sh.FolderID)
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}
	n, err := s.db.Q.CountShareImages(ctx, sh.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "share", sh.Name, SharePage{
		Share: sh, Folder: f, URL: s.publicURL("s", sh.Token), Expired: sh.ExpiresAt <= database.Time(time.Now()),
		Count: n, DefaultDays: s.shareDefaultDays(), CSRF: sessionFrom(ctx).CSRFToken, Grid: g,
	})
}

func (s *Server) shareRename(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.share(w, r)
	if !ok {
		return
	}
	name := strings.Join(strings.Fields(r.PostFormValue("name")), " ")
	if name == "" || len([]rune(name)) > 100 {
		s.fail(w, r, http.StatusBadRequest, "err.share_name")
		return
	}
	if _, err := s.db.Q.RenameShare(r.Context(), sqlc.RenameShareParams{Name: name, ID: sh.ID}); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/shares/"+sh.ID)
}

// shareExtend sets a new expiry, counted from now; the link stays the same.
func (s *Server) shareExtend(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.share(w, r)
	if !ok {
		return
	}
	exp := database.Time(time.Now().Add(s.shareDuration(r)))
	if _, err := s.db.Q.ExtendShare(r.Context(), sqlc.ExtendShareParams{ExpiresAt: exp, ID: sh.ID}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("share expiry changed", "share_id", sh.ID, "user_id", sessionFrom(r.Context()).UserID, "expires_at", exp)
	redirect(w, r, "/shares/"+sh.ID)
}

func (s *Server) shareDelete(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.share(w, r)
	if !ok {
		return
	}
	if err := s.db.Q.DeleteShare(r.Context(), sh.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("share deleted", "share_id", sh.ID, "folder_id", sh.FolderID, "user_id", sessionFrom(r.Context()).UserID)
	redirect(w, r, "/folders/"+sh.FolderID)
}

// shareRemoveImages takes the checked images out of the share (they stay in the folder).
func (s *Server) shareRemoveImages(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.share(w, r)
	if !ok {
		return
	}
	ids, ok := s.selectedImages(w, r)
	if !ok {
		return
	}
	n, err := s.db.Q.RemoveShareImages(r.Context(), sqlc.RemoveShareImagesParams{ShareID: sh.ID, ImageIds: ids})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("images unshared", "share_id", sh.ID, "user_id", sessionFrom(r.Context()).UserID, "count", n)
	redirect(w, r, "/shares/"+sh.ID)
}

// shareGrid returns the page of the share's images after seq. token is set
// for the public page, which links tiles to the share's own routes.
func (s *Server) shareGrid(ctx context.Context, sh sqlc.Share, token string, after int64) (ShareGridView, error) {
	imgs, err := s.db.Q.ListShareImages(ctx, sqlc.ListShareImagesParams{ShareID: sh.ID, AfterSeq: after, PageSize: pageSize + 1})
	if err != nil {
		return ShareGridView{}, err
	}
	g := ShareGridView{Token: token}
	if len(imgs) > pageSize {
		imgs = imgs[:pageSize]
		next := "?after=" + strconv.FormatInt(imgs[len(imgs)-1].Seq, 10)
		if token != "" {
			g.NextURL = "/s/" + url.PathEscape(token) + next
		} else {
			g.NextURL = "/shares/" + sh.ID + next
		}
	}
	for _, im := range imgs {
		g.Images = append(g.Images, tile(im))
	}
	return g, nil
}

// ---- public (share link) ----

// publicShare resolves the token to a usable share. It renders the error page
// (invalid / expired) and returns false if the link cannot be used.
func (s *Server) publicShare(w http.ResponseWriter, r *http.Request) (sqlc.Share, bool) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	sh, err := s.db.Q.GetShareByToken(r.Context(), r.PathValue("token"))
	if errors.Is(err, sql.ErrNoRows) {
		s.page(w, r, http.StatusNotFound, "share_invalid", "Link not found", nil)
		return sh, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return sh, false
	}
	if sh.ExpiresAt <= database.Time(time.Now()) {
		s.page(w, r, http.StatusGone, "share_expired", "Link expired", nil)
		return sh, false
	}
	return sh, true
}

// publicShareImage resolves the token and the image, which must be part of the share.
func (s *Server) publicShareImage(w http.ResponseWriter, r *http.Request) (sqlc.Share, sqlc.Image, bool) {
	sh, ok := s.publicShare(w, r)
	if !ok {
		return sh, sqlc.Image{}, false
	}
	img, err := s.db.Q.GetShareImage(r.Context(), sqlc.GetShareImageParams{ShareID: sh.ID, ImageID: r.PathValue("id")})
	if err != nil {
		s.notFoundOr(w, r, err)
		return sh, img, false
	}
	return sh, img, true
}

func (s *Server) publicShareShow(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.publicShare(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	g, err := s.shareGrid(ctx, sh, sh.Token, after)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if after > 0 {
		s.fragment(w, r, "share_grid", g)
		return
	}
	n, err := s.db.Q.CountShareImages(ctx, sh.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "share_public", sh.Name, PublicSharePage{
		Token: sh.Token, Name: sh.Name, ExpiresAt: sh.ExpiresAt, Count: n, Grid: g,
	})
}

func (s *Server) publicShareImageShow(w http.ResponseWriter, r *http.Request) {
	sh, img, ok := s.publicShareImage(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	v := PublicImageView{TileView: tile(img), Token: sh.Token, ShareName: sh.Name}
	var err error
	if v.Prev, err = neighbour(s.db.Q.PrevShareImageID(ctx, sqlc.PrevShareImageIDParams{ShareID: sh.ID, Seq: img.Seq})); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Next, err = neighbour(s.db.Q.NextShareImageID(ctx, sqlc.NextShareImageIDParams{ShareID: sh.ID, Seq: img.Seq})); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "share_image", img.OriginalFilename, v)
}

func (s *Server) publicShareFile(w http.ResponseWriter, r *http.Request) {
	_, img, ok := s.publicShareImage(w, r)
	if !ok {
		return
	}
	// Not cached for long: the file should stop loading soon after the link
	// expires or the image is taken out of the share.
	s.serveImage(w, r, img, r.PathValue("kind"), "private, max-age=600")
}

// publicShareZip streams the originals of the share as a ZIP. Unlike the
// export for signed-in users it has no XMP sidecars: ratings, tags and
// nicknames stay private.
func (s *Server) publicShareZip(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.publicShare(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	ids, err := s.db.Q.ListShareImageIDs(ctx, sh.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(ids) == 0 { // an empty list would export the whole folder
		http.NotFound(w, r)
		return
	}
	exp, err := s.downloads.Prepare(ctx, sh.FolderID, ids)
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}
	exp.Dir = domain.SafeFilename(sh.Name)
	for i := range exp.Files {
		exp.Files[i].XMP = nil
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+domain.Slug(sh.Name)+`.zip"`)
	w.Header().Set("Content-Length", strconv.FormatInt(exp.ZipSize(), 10))
	w.Header().Set("Cache-Control", "private, no-store")
	s.log.Info("share download started", "share_id", sh.ID, "files", len(exp.Files))
	if err := s.downloads.WriteZip(ctx, w, exp); err != nil {
		if ctx.Err() != nil {
			s.log.Info("share download cancelled by client", "share_id", sh.ID)
		} else {
			s.log.Error("share download failed", "share_id", sh.ID, "err", err)
		}
		panic(http.ErrAbortHandler)
	}
}
