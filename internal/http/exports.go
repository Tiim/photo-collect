package http

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/downloads"
)

// prepareExport reads which images to export and collects them: with
// matching=1 in the query everything matching the filter there, with all=1
// (query or form) the whole folder, otherwise the "image" fields. It writes
// the error response itself when ok is false.
func (s *Server) prepareExport(w http.ResponseWriter, r *http.Request) (exp *downloads.Export, ok bool) {
	f, ok := s.folder(w, r)
	if !ok {
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "err.bad_request")
		return nil, false
	}
	var ids []string
	if r.URL.Query().Get("matching") == "1" {
		// Everything that matches the filter in the query string, resolved now.
		flt, ok := s.parseFilter(w, r)
		if !ok {
			return nil, false
		}
		var err error
		if ids, err = s.matchingIDs(r.Context(), f.ID, flt); err != nil {
			s.serverError(w, r, err)
			return nil, false
		}
		if len(ids) == 0 {
			s.fail(w, r, http.StatusBadRequest, "err.filter_none")
			return nil, false
		}
	} else if r.Form.Get("all") != "1" {
		ids = r.Form["image"]
		if len(ids) == 0 {
			s.fail(w, r, http.StatusBadRequest, "err.select_one")
			return nil, false
		}
	}
	exp, err := s.downloads.Prepare(r.Context(), f.ID, ids)
	if err != nil {
		s.notFoundOr(w, r, err)
		return nil, false
	}
	return exp, true
}

// exportZip streams the originals and their XMP sidecars as a ZIP. Nothing is
// stored on the server; the length is known up front, so browsers show progress.
func (s *Server) exportZip(w http.ResponseWriter, r *http.Request) {
	exp, ok := s.prepareExport(w, r)
	if !ok {
		return
	}
	sess := sessionFrom(r.Context())
	name := domain.Slug(exp.FolderName) + "-" + exp.Created.Format("2006-01-02") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(exp.ZipSize(), 10))
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method == http.MethodHead {
		return
	}
	s.log.Info("export started", "folder_id", exp.FolderID, "user_id", sess.UserID, "files", len(exp.Files))
	if err := s.downloads.WriteZip(r.Context(), w, exp); err != nil {
		if r.Context().Err() != nil {
			s.log.Info("export cancelled by client", "folder_id", exp.FolderID, "user_id", sess.UserID)
		} else {
			s.log.Error("export failed", "folder_id", exp.FolderID, "user_id", sess.UserID, "err", err)
		}
		// Headers are out: drop the connection so the client sees a failed
		// download rather than a short archive.
		panic(http.ErrAbortHandler)
	}
}

type exportFileJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	URL       string `json:"url"`
	XMPName   string `json:"xmp_name"`
	XMP       string `json:"xmp"`
	XMPSHA256 string `json:"xmp_sha256"`
}

// exportJSON lists the files of an export for "save to folder": the page
// downloads each original from its URL and writes the sidecar itself. Names
// are relative to dir and stable per image, so a later run can skip what a
// folder already has.
func (s *Server) exportJSON(w http.ResponseWriter, r *http.Request) {
	exp, ok := s.prepareExport(w, r)
	if !ok {
		return
	}
	files := make([]exportFileJSON, len(exp.Files))
	for i, f := range exp.Files {
		sum := sha256.Sum256(f.XMP)
		files[i] = exportFileJSON{
			ID: f.ImageID, Name: f.Name, Size: f.Size, SHA256: f.SHA256,
			URL:     "/images/" + f.ImageID + "/original",
			XMPName: f.XMPName(), XMP: string(f.XMP), XMPSHA256: hex.EncodeToString(sum[:]),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Dir   string           `json:"dir"`
		Files []exportFileJSON `json:"files"`
	}{exp.Dir, files})
}
