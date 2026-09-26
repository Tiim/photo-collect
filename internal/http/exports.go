package http

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/domain"
)

func (s *Server) exportsView(ctx context.Context, folderID string) (ExportsView, error) {
	exps, err := s.db.Q.ListFolderExports(ctx, folderID)
	if err != nil {
		return ExportsView{}, err
	}
	v := ExportsView{FolderID: folderID}
	nowStr := database.Time(time.Now())
	for _, e := range exps {
		if e.ExpiresAt <= nowStr {
			continue // about to be cleaned up
		}
		v.Exports = append(v.Exports, ExportRow{Export: e, Ready: e.Status == "ready"})
		if e.Status == "pending" || e.Status == "running" {
			v.Active = true
		}
	}
	return v, nil
}

func (s *Server) exportsPanel(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	v, err := s.exportsView(r.Context(), f.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "exports", v)
}

// exportCreate queues an export of the selected images, or all with all=1.
func (s *Server) exportCreate(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	var ids []string
	if r.PostFormValue("all") != "1" {
		ids = r.PostForm["image"]
		if len(ids) == 0 {
			http.Error(w, "Select at least one image", http.StatusBadRequest)
			return
		}
	}
	sess := sessionFrom(r.Context())
	exp, err := s.downloads.Create(r.Context(), f.ID, sess.UserID, ids)
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}
	s.log.Info("export requested", "export_id", exp.ID, "folder_id", f.ID, "user_id", sess.UserID, "selected", len(ids))
	v, err := s.exportsView(r.Context(), f.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "exports", v)
}

func (s *Server) exportDownload(w http.ResponseWriter, r *http.Request) {
	exp, err := s.db.Q.GetExport(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}
	folder, err := s.db.Q.GetFolder(r.Context(), exp.FolderID)
	if err != nil {
		s.notFoundOr(w, r, err)
		return
	}
	if exp.Status != "ready" || exp.ExpiresAt <= database.Time(time.Now()) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.downloads.FilePath(exp))
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	created, _ := database.ParseTime(exp.CreatedAt)
	name := domain.Slug(folder.Name) + "-" + created.Format("2006-01-02") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
