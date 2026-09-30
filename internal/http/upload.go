package http

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/uploads"
)

const nicknameCookie = "nickname"

type UploadPage struct {
	Token      string
	FolderName string
	Nickname   string // empty until the visitor has chosen one
	Error      string
	MaxFiles   int
	MaxSizeMB  int64
}

type uploadResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// uploadLink resolves the token to a usable link. It renders the appropriate
// error page (invalid / expired) and returns false if the link cannot be used.
func (s *Server) uploadLink(w http.ResponseWriter, r *http.Request) (sqlc.GetUploadLinkByTokenRow, bool) {
	l, err := s.db.Q.GetUploadLinkByToken(r.Context(), r.PathValue("token"))
	if errors.Is(err, sql.ErrNoRows) {
		s.page(w, r, http.StatusNotFound, "upload_invalid", "Link not found", nil)
		return l, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return l, false
	}
	if l.ExpiresAt <= database.Time(time.Now()) {
		s.page(w, r, http.StatusGone, "upload_expired", "Link expired", l.FolderName)
		return l, false
	}
	return l, true
}

func (s *Server) nickname(r *http.Request) string {
	c, err := r.Cookie(nicknameCookie)
	if err != nil {
		return ""
	}
	v, err := s.signer.Verify(nicknameCookie, c.Value)
	if err != nil {
		return ""
	}
	nick, err := domain.CleanNickname(v)
	if err != nil {
		return ""
	}
	return nick
}

func (s *Server) uploadPage(w http.ResponseWriter, r *http.Request) {
	l, ok := s.uploadLink(w, r)
	if !ok {
		return
	}
	s.renderUploadPage(w, r, l, s.nickname(r), "", http.StatusOK)
}

func (s *Server) renderUploadPage(w http.ResponseWriter, r *http.Request, l sqlc.GetUploadLinkByTokenRow, nick, errMsg string, status int) {
	s.page(w, r, status, "upload", l.FolderName, UploadPage{
		Token: l.Token, FolderName: l.FolderName, Nickname: nick, Error: errMsg,
		MaxFiles: s.cfg.UploadMaxFilesPerRequest, MaxSizeMB: s.cfg.UploadMaxFileSize >> 20,
	})
}

func (s *Server) uploadSetNickname(w http.ResponseWriter, r *http.Request) {
	l, ok := s.uploadLink(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	nick, err := domain.CleanNickname(r.PostFormValue("nickname"))
	if err != nil {
		s.renderUploadPage(w, r, l, "", err.Error(), http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: nicknameCookie, Value: s.signer.Sign(nicknameCookie, nick), Path: "/upload/",
		Expires:  time.Now().AddDate(10, 0, 0),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/upload/"+l.Token, http.StatusSeeOther)
}

func (s *Server) uploadClearNickname(w http.ResponseWriter, r *http.Request) {
	l, ok := s.uploadLink(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: nicknameCookie, Value: "", Path: "/upload/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/upload/"+l.Token, http.StatusSeeOther)
}

// uploadImages accepts multipart uploads, streaming each file part straight to
// disk/storage. The page's script sends one file per request so it can show
// per-file progress; several files per request are also supported.
func (s *Server) uploadImages(w http.ResponseWriter, r *http.Request) {
	l, ok := s.uploadLink(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	nick := s.nickname(r)
	if nick == "" {
		writeJSON(w, http.StatusForbidden, []uploadResult{{Error: "Please enter a nickname first"}})
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		http.Error(w, "Expected multipart/form-data", http.StatusBadRequest)
		return
	}
	// Cap the whole request; parts are also capped individually by the ingest service.
	maxBody := int64(s.cfg.UploadMaxFilesPerRequest)*s.cfg.UploadMaxFileSize + (1 << 20)
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	mr := multipart.NewReader(r.Body, params["boundary"])

	var results []uploadResult
	status := http.StatusOK
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.log.Warn("upload: reading multipart failed", "err", err)
			results = append(results, uploadResult{Error: "The upload was interrupted"})
			status = http.StatusBadRequest
			break
		}
		if part.FormName() != "files" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := domain.SafeFilename(part.FileName())
		if len(results) >= s.cfg.UploadMaxFilesPerRequest {
			results = append(results, uploadResult{Name: name, Error: "Too many files in one upload"})
			part.Close()
			continue
		}
		res := uploadResult{Name: name}
		if _, err := s.uploads.Ingest(r.Context(), l.FolderID, nick, name, part); err != nil {
			res.Error = s.uploadErrorMessage(err, l.FolderID, name)
		} else {
			res.OK = true
			s.log.Info("image uploaded", "folder_id", l.FolderID)
		}
		results = append(results, res)
		part.Close()
	}
	writeJSON(w, status, results)
}

func (s *Server) uploadErrorMessage(err error, folderID, name string) string {
	msg := ""
	switch {
	case errors.Is(err, uploads.ErrTooLarge):
		msg = fmt.Sprintf("File is too large (max %d MB)", s.cfg.UploadMaxFileSize>>20)
	case errors.Is(err, uploads.ErrFolderFull):
		msg = uploads.ErrFolderFull.Error()
	case errors.Is(err, uploads.ErrFolderGone):
		msg = uploads.ErrFolderGone.Error()
	case errors.Is(err, uploads.ErrEmptyFile):
		msg = uploads.ErrEmptyFile.Error()
	case errors.Is(err, images.ErrAnimated):
		msg = images.ErrAnimated.Error()
	case errors.Is(err, images.ErrNotAnImage), errors.Is(err, images.ErrUnsupported):
		msg = images.ErrNotAnImage.Error()
	case errors.Is(err, images.ErrTooManyPixels):
		msg = "Image resolution is too large"
	case errors.Is(err, images.ErrCorrupt):
		msg = images.ErrCorrupt.Error()
	}
	if msg != "" {
		s.log.Info("upload rejected", "folder_id", folderID, "reason", err.Error())
		return msg
	}
	s.log.Error("upload failed", "folder_id", folderID, "err", err)
	return "Upload failed, please try again"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"results": v})
}
