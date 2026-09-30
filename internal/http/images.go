package http

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/storage"
)

type ImageView struct {
	TileView
	Folder sqlc.Folder
	Tags   TagsView
}

type TagsView struct {
	ImageID string
	Tags    []sqlc.Tag
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) (sqlc.Image, bool) {
	img, err := s.db.Q.GetImage(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOr(w, r, err)
		return img, false
	}
	return img, true
}

func (s *Server) imageShow(w http.ResponseWriter, r *http.Request) {
	img, ok := s.image(w, r)
	if !ok {
		return
	}
	folder, err := s.db.Q.GetFolder(r.Context(), img.FolderID)
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}
	tags, err := s.db.Q.ListImageTags(r.Context(), img.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v := ImageView{TileView: tile(img), Folder: folder, Tags: TagsView{ImageID: img.ID, Tags: tags}}
	if isHTMX(r) {
		s.fragment(w, r, "image_detail", v)
		return
	}
	s.page(w, r, http.StatusOK, "image", img.OriginalFilename, v)
}

func (s *Server) imageTile(w http.ResponseWriter, r *http.Request) {
	img, ok := s.image(w, r)
	if !ok {
		return
	}
	s.fragment(w, r, "image_tile", tile(img))
}

// imageFile proxies image bytes from storage. Nothing is ever served without a session.
func (s *Server) imageFile(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		img, ok := s.image(w, r)
		if !ok {
			return
		}
		var key, contentType string
		var length int64 = -1
		switch kind {
		case "original":
			key, contentType, length = storage.OriginalKey(img.FolderID, img.ID), img.MimeType, img.SizeBytes
		case "preview":
			if img.PreviewReady == 0 {
				http.NotFound(w, r)
				return
			}
			key, contentType = storage.PreviewKey(img.FolderID, img.ID), "image/jpeg"
		case "thumbnail":
			if img.ThumbnailReady == 0 {
				http.NotFound(w, r)
				return
			}
			key, contentType = storage.ThumbnailKey(img.FolderID, img.ID), "image/jpeg"
		}
		// Originals are immutable; derivatives can be regenerated, so their
		// ETag carries the derivative version.
		etag := fmt.Sprintf(`"%s-%s"`, img.Sha256[:16], kind)
		if kind != "original" {
			etag = fmt.Sprintf(`"%s-%s-%d"`, img.Sha256[:16], kind, img.DerivativeVersion)
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		if etagMatches(r.Header.Values("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		rc, err := s.store.Get(r.Context(), key)
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.log.Error("storage read failed", "key", key, "err", err)
			http.Error(w, "Storage unavailable", http.StatusBadGateway)
			return
		}
		defer rc.Close()

		w.Header().Set("Content-Type", contentType)
		if length >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		}
		if kind == "original" {
			disp := "inline"
			if r.URL.Query().Get("download") == "1" {
				disp = "attachment"
			}
			w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": asciiName(img.OriginalFilename)}))
		}
		_, _ = io.Copy(w, rc)
	}
}

// etagMatches implements the weak comparison of If-None-Match: the header may
// be a comma-separated list, contain weak ("W/") validators or be "*".
func etagMatches(headers []string, etag string) bool {
	for _, h := range headers {
		for _, v := range strings.Split(h, ",") {
			v = strings.TrimPrefix(strings.TrimSpace(v), "W/")
			if v == "*" || v == etag {
				return true
			}
		}
	}
	return false
}

// asciiName gives a header-safe fallback filename.
func asciiName(n string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return '_'
		}
		return r
	}, n)
}

func (s *Server) imageRate(w http.ResponseWriter, r *http.Request) {
	img, ok := s.image(w, r)
	if !ok {
		return
	}
	v, err := strconv.Atoi(r.PostFormValue("rating"))
	if err != nil || v < 0 || v > 5 {
		http.Error(w, "Rating must be between 0 and 5", http.StatusBadRequest)
		return
	}
	rating := sql.NullInt64{Int64: int64(v), Valid: v > 0}
	if _, err := s.db.Q.SetImageRating(r.Context(), sqlc.SetImageRatingParams{Rating: rating, ID: img.ID}); err != nil {
		s.serverError(w, r, err)
		return
	}
	img.Rating = rating
	s.fragment(w, r, "rating_response", tile(img))
}

func (s *Server) tagsResponse(w http.ResponseWriter, r *http.Request, imageID string) {
	tags, err := s.db.Q.ListImageTags(r.Context(), imageID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "image_tags", TagsView{ImageID: imageID, Tags: tags})
}

func (s *Server) imageTagAdd(w http.ResponseWriter, r *http.Request) {
	img, ok := s.image(w, r)
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
		return q.AddImageTag(r.Context(), sqlc.AddImageTagParams{ImageID: img.ID, TagID: t.ID})
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.tagsResponse(w, r, img.ID)
}

func (s *Server) imageTagRemove(w http.ResponseWriter, r *http.Request) {
	img, ok := s.image(w, r)
	if !ok {
		return
	}
	tagID, err := strconv.ParseInt(r.PathValue("tag"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.db.Q.RemoveImageTag(r.Context(), sqlc.RemoveImageTagParams{ImageID: img.ID, TagID: tagID}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.tagsResponse(w, r, img.ID)
}

// tagSuggest returns <option> elements for the tag autocomplete datalist.
func (s *Server) tagSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("name")))
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	names, err := s.db.Q.SuggestTags(r.Context(), sql.NullString{String: esc, Valid: true})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "tag_options", names)
}
