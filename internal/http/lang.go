package http

import (
	"context"
	"html/template"
	"net/http"
	"net/url"

	"github.com/tiim/photo-collect/internal/i18n"
)

const langCookie = "lang"

type langKey struct{}

func langFrom(ctx context.Context) string {
	if l, ok := ctx.Value(langKey{}).(string); ok {
		return l
	}
	return i18n.Default
}

// lang resolves the request language: ?lang= (also sets the cookie), the lang
// cookie, Accept-Language, then the default.
func (s *Server) lang(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var lang string
		if q := r.URL.Query().Get("lang"); s.i18n.Supported(q) {
			lang = q
			http.SetCookie(w, &http.Cookie{
				Name: langCookie, Value: q, Path: "/", MaxAge: 365 * 24 * 3600,
				HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
			})
		} else if c, err := r.Cookie(langCookie); err == nil && s.i18n.Supported(c.Value) {
			lang = c.Value
		} else {
			lang = s.i18n.Match(r.Header.Get("Accept-Language"))
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), langKey{}, lang)))
	})
}

// translator returns the message function for the request's language.
func (s *Server) translator(r *http.Request) *i18n.Localizer {
	return s.i18n.Localizer(langFrom(r.Context()))
}

// tHTML is like T but returns HTML: the message is trusted (it comes from the
// embedded catalogs and may contain markup), string arguments are escaped.
func tHTML(l *i18n.Localizer, id string, data ...any) template.HTML {
	if len(data) > 0 {
		if m, ok := data[0].(map[string]any); ok {
			esc := make(map[string]any, len(m))
			for k, v := range m {
				if str, ok := v.(string); ok {
					v = template.HTMLEscapeString(str)
				}
				esc[k] = v
			}
			data = append([]any{esc}, data[1:]...)
		}
	}
	return template.HTML(l.T(id, data...))
}

// LangOption is one entry of the language switcher.
type LangOption struct {
	Code, Name, URL string
	Current         bool
}

// langNames are the endonyms shown in the switcher.
var langNames = map[string]string{"en": "English", "de": "Deutsch"}

func (s *Server) langOptions(r *http.Request, current string) []LangOption {
	var out []LangOption
	for _, code := range s.i18n.Languages() {
		q := url.Values{}
		for k, v := range r.URL.Query() {
			q[k] = v
		}
		q.Set("lang", code)
		name := langNames[code]
		if name == "" {
			name = code
		}
		out = append(out, LangOption{Code: code, Name: name, URL: r.URL.Path + "?" + q.Encode(), Current: code == current})
	}
	return out
}
