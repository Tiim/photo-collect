// Package http contains the HTTP handlers, middleware and templates of the web UI.
package http

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/tiim/photo-collect/internal/clientip"
	"github.com/tiim/photo-collect/internal/config"
	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/downloads"
	"github.com/tiim/photo-collect/internal/i18n"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/library"
	"github.com/tiim/photo-collect/internal/oidc"
	"github.com/tiim/photo-collect/internal/ratelimit"
	"github.com/tiim/photo-collect/internal/sessions"
	"github.com/tiim/photo-collect/internal/storage"
	"github.com/tiim/photo-collect/internal/uploads"
	"github.com/tiim/photo-collect/web"
)

type Server struct {
	cfg       *config.Config
	db        *database.DB
	store     storage.Store
	sessions  *sessions.Manager
	signer    *sessions.Signer
	oidc      *oidc.Handler
	uploads   *uploads.Service
	chunks    *uploads.Chunks
	downloads *downloads.Service
	library   *library.Service
	queue     *jobs.Queue
	log       *slog.Logger
	pages     map[string]map[string]*template.Template // language -> page name -> templates
	secure    bool
	i18n      *i18n.Bundle

	clientIPs *clientip.Resolver
	warnProxy sync.Once

	limUploadIP, limUploadLink, limAuthIP, limNicknameIP *ratelimit.Limiter
	ingestSlots                                          chan struct{} // nil = unlimited
	static                                               *staticAssets
}

type Deps struct {
	Config    *config.Config
	DB        *database.DB
	Store     storage.Store
	Sessions  *sessions.Manager
	Signer    *sessions.Signer
	OIDC      *oidc.Handler
	Uploads   *uploads.Service
	Chunks    *uploads.Chunks
	Downloads *downloads.Service
	Queue     *jobs.Queue
	Log       *slog.Logger
}

func NewServer(d Deps) (*Server, error) {
	s := &Server{
		cfg: d.Config, db: d.DB, store: d.Store, sessions: d.Sessions, signer: d.Signer,
		oidc: d.OIDC, uploads: d.Uploads, chunks: d.Chunks, downloads: d.Downloads, library: library.New(d.DB, d.Queue), queue: d.Queue, log: d.Log,
		secure:    strings.HasPrefix(d.Config.BaseURL, "https://"),
		clientIPs: clientip.NewResolver(d.Config.TrustedProxies),

		limUploadIP:   ratelimit.New(d.Config.RateUploadPerIP),
		limUploadLink: ratelimit.New(d.Config.RateUploadPerLink),
		limAuthIP:     ratelimit.New(d.Config.RateAuthPerIP),
		limNicknameIP: ratelimit.New(d.Config.RateNicknamePerIP),
	}
	if n := d.Config.UploadMaxConcurrent; n > 0 {
		s.ingestSlots = make(chan struct{}, n)
	}
	bundle, err := i18n.New(web.FS, "locales", d.Log)
	if err != nil {
		return nil, err
	}
	s.i18n = bundle
	staticFS, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}
	if s.static, err = newStaticAssets(staticFS); err != nil {
		return nil, err
	}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /readyz", s.readyz)

	authLimit := []limitRule{{"auth-ip", s.limAuthIP, s.ipKey}}
	mux.Handle("GET /auth/login", s.limited(http.HandlerFunc(s.oidc.Login), authLimit...))
	mux.Handle("GET /auth/callback", s.limited(http.HandlerFunc(s.oidc.Callback), authLimit...))
	mux.Handle("POST /auth/logout", s.auth(s.logout))

	// Anonymous upload (capability = possession of the link token).
	uploadLimit := []limitRule{{"upload-ip", s.limUploadIP, s.ipKey}, {"upload-link", s.limUploadLink, tokenKey}}
	nickLimit := append([]limitRule{{"nickname-ip", s.limNicknameIP, s.ipKey}}, uploadLimit...)
	mux.Handle("GET /upload/{token}", s.limited(http.HandlerFunc(s.uploadPage), uploadLimit...))
	mux.Handle("POST /upload/{token}/nickname", s.limited(http.HandlerFunc(s.uploadSetNickname), nickLimit...))
	mux.Handle("POST /upload/{token}/nickname/clear", s.limited(http.HandlerFunc(s.uploadClearNickname), nickLimit...))
	mux.Handle("POST /upload/{token}/chunked", s.limited(http.HandlerFunc(s.chunkedStart), uploadLimit...))
	mux.HandleFunc("PUT /upload/{token}/chunked/{id}", s.chunkedPut)
	mux.Handle("POST /upload/{token}/chunked/{id}/complete", s.limited(http.HandlerFunc(s.chunkedComplete), uploadLimit...))

	// Public clock page (photographed to calibrate camera clocks) and its time source.
	mux.HandleFunc("GET /{$}", s.clockPage)
	mux.HandleFunc("GET /time", s.serverTime)

	// Authenticated UI.
	mux.Handle("GET /folders", s.auth(s.foldersList))
	mux.Handle("POST /folders", s.auth(s.folderCreate))
	mux.Handle("GET /folders/{id}", s.auth(s.folderShow))
	mux.Handle("GET /folders/{id}/gallery", s.auth(s.folderGallery))
	mux.Handle("GET /folders/{id}/images", s.auth(s.folderImages))
	mux.Handle("GET /folders/{id}/map.json", s.auth(s.folderMapJSON))
	mux.Handle("POST /folders/{id}/images/trash", s.auth(s.imagesTrash))
	mux.Handle("POST /folders/{id}/images/restore", s.auth(s.trashChange(false)))
	mux.Handle("POST /folders/{id}/images/purge", s.auth(s.trashChange(true)))
	mux.Handle("GET /folders/{id}/trash", s.auth(s.trashShow))
	mux.Handle("POST /folders/{id}/rename", s.auth(s.folderRename))
	mux.Handle("POST /folders/{id}/delete", s.auth(s.folderDelete))
	mux.Handle("POST /folders/{id}/standard-tags", s.auth(s.standardTagAdd))
	mux.Handle("POST /folders/{id}/standard-tags/{tag}/delete", s.auth(s.standardTagRemove))
	mux.Handle("POST /folders/{id}/upload-link", s.auth(s.uploadLinkCreate))
	mux.Handle("POST /folders/{id}/upload-link/extend", s.auth(s.uploadLinkExtend))
	mux.Handle("POST /folders/{id}/upload-link/revoke", s.auth(s.uploadLinkRevoke))
	mux.Handle("GET /folders/{id}/downloads", s.auth(s.exportsPanel))
	mux.Handle("POST /folders/{id}/downloads", s.auth(s.exportCreate))
	mux.Handle("GET /downloads/{id}", s.auth(s.exportDownload))
	mux.Handle("POST /duplicates/{id}/dismiss", s.auth(s.duplicateDismiss))
	mux.Handle("POST /duplicates/{id}/resolve", s.auth(s.duplicateResolve))

	mux.Handle("GET /images/{id}", s.auth(s.imageShow))
	mux.Handle("GET /images/{id}/tile", s.auth(s.imageTile))
	mux.Handle("GET /images/{id}/thumbnail", s.auth(s.imageFile("thumbnail")))
	mux.Handle("GET /images/{id}/preview", s.auth(s.imageFile("preview")))
	mux.Handle("GET /images/{id}/original", s.auth(s.imageFile("original")))
	mux.Handle("POST /images/{id}/rating", s.auth(s.imageRate))
	mux.Handle("POST /images/{id}/trash", s.auth(s.imageTrash))
	mux.Handle("POST /images/{id}/tags", s.auth(s.imageTagAdd))
	mux.Handle("POST /images/{id}/tags/{tag}/delete", s.auth(s.imageTagRemove))
	mux.Handle("GET /tags/suggest", s.auth(s.tagSuggest))

	return chain(mux, s.recoverer, s.securityHeaders, s.lang, s.accessLog, s.realIP)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		s.log.Error("readiness: database", "err", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.store.Ping(ctx); err != nil {
		s.log.Error("readiness: storage", "err", err)
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ready")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Destroy(w, r)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

// ---- helpers ----

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "route", routeLabel(r), "err", err)
	s.fail(w, r, http.StatusInternalServerError, "err.internal")
}

// notFoundOr responds 404 for sql.ErrNoRows and 500 otherwise.
func (s *Server) notFoundOr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	s.serverError(w, r, err)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// redirect sends the browser to loc, using HX-Redirect for htmx requests.
func redirect(w http.ResponseWriter, r *http.Request, loc string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", loc)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

func (s *Server) publicURL(p ...string) string {
	return s.cfg.BaseURL + "/" + path.Join(p...)
}

func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser or same-origin GET-style request
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	b, err := url.Parse(s.cfg.BaseURL)
	return err == nil && u.Scheme == b.Scheme && u.Host == b.Host
}

func (s *Server) execute(name string, tmpl *template.Template, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
