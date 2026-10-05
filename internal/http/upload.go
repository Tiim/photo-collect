package http

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tiim/photo-collect/internal/clientip"
	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
)

const nicknameCookie = "nickname"

type UploadPage struct {
	Token      string
	FolderName string
	Nickname   string // empty until the visitor has chosen one
	Error      string
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
		MaxSizeMB: s.cfg.UploadMaxFileSize >> 20,
	})
}

func (s *Server) uploadSetNickname(w http.ResponseWriter, r *http.Request) {
	l, ok := s.uploadLink(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "err.forbidden")
		return
	}
	nick, err := domain.CleanNickname(r.PostFormValue("nickname"))
	if err != nil {
		msg, _ := s.errText(r, err)
		s.renderUploadPage(w, r, l, "", msg, http.StatusBadRequest)
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
		s.fail(w, r, http.StatusForbidden, "err.forbidden")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: nicknameCookie, Value: "", Path: "/upload/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/upload/"+l.Token, http.StatusSeeOther)
}

// acquireIngestSlot bounds the number of uploads being ingested at once so
// parallel requests cannot fill the temp directory; excess clients get 503 and
// retry shortly.
func (s *Server) acquireIngestSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	if s.ingestSlots == nil {
		return func() {}, true
	}
	select {
	case s.ingestSlots <- struct{}{}:
		return func() { <-s.ingestSlots }, true
	default:
		s.log.Info("upload rejected: server busy", "route", routeLabel(r), "remote", clientip.String(r.Context()))
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, []uploadResult{{Error: s.translator(r).T("err.upload.busy")}})
		return nil, false
	}
}

func (s *Server) uploadErrorMessage(r *http.Request, err error, folderID, name string) string {
	if msg, ok := s.errText(r, err); ok {
		s.log.Info("upload rejected", "folder_id", folderID, "reason", err.Error())
		return msg
	}
	s.log.Error("upload failed", "folder_id", folderID, "err", err)
	return s.translator(r).T("err.upload.failed")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"results": v})
}
