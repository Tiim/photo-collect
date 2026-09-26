package http

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/sessions"
)

type ctxKey struct{}

func sessionFrom(ctx context.Context) *sessions.Session {
	s, _ := ctx.Value(ctxKey{}).(*sessions.Session)
	return s
}

func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for _, mw := range mws {
		h = mw(h)
	}
	return h
}

// auth requires a signed-in user and, for unsafe methods, a valid CSRF token
// and same-origin request.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.sessions.Get(r)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if sess == nil {
			if isHTMX(r) {
				w.Header().Set("HX-Redirect", "/auth/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				tok = r.PostFormValue("csrf")
			}
			if !s.sameOrigin(r) || subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRFToken)) != 1 {
				s.log.Warn("csrf check failed", "route", routeLabel(r), "user_id", sess.UserID)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic in handler", "route", routeLabel(r), "panic", rec, "stack", string(debug.Stack()))
				http.Error(w, "Something went wrong", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// Upload links are bearer tokens in the URL: never leak them via Referer.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
		if s.secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		s.log.Info("request", "method", r.Method, "route", routeLabel(r), "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

// routeLabel returns the matched route pattern. It deliberately never logs the
// raw path, because upload links carry a secret token in the URL.
func routeLabel(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	if strings.HasPrefix(r.URL.Path, "/upload/") {
		return "/upload/[redacted]"
	}
	return r.URL.Path
}
