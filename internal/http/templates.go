package http

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/i18n"
	"github.com/tiim/photo-collect/internal/sessions"
	"github.com/tiim/photo-collect/web"
)

// dateLayouts are the date formats per language; anything else uses ISO order.
var dateLayouts = map[string]string{"de": "02.01.2006 15:04"}

// jsKeys are the catalog entries exposed to the scripts in web/static.
var jsKeys = []string{
	"js.copied", "detail.prev_hint", "detail.next_hint", "js.waiting", "js.uploading", "js.uploaded", "js.already_uploaded", "js.network_error", "js.busy_retry",
	"js.busy_give_up", "js.expired", "js.too_large", "js.failed", "js.network_retry", "js.processing", "clock.synced", "clock.unreachable",
}

// localeFuncs returns the template functions for one language. Templates are
// parsed once per language so that partials rendered as fragments translate
// without carrying a translator in their data.
func localeFuncs(loc *i18n.Localizer) template.FuncMap {
	lang := loc.Lang()
	layout := dateLayouts[lang]
	if layout == "" {
		layout = "2006-01-02 15:04"
	}
	return template.FuncMap{
		// t translates a message ID: {{t "id"}} or {{t "id" (dict "Name" .X)}}.
		"t": func(id string, data ...any) string { return loc.T(id, data...) },
		// th is t for messages containing markup; string arguments are escaped.
		"th": func(id string, data ...any) template.HTML { return tHTML(loc, id, data...) },
		// jsStrings is the JSON block read by app.js and clock.js.
		"jsStrings": func() template.JS {
			m := make(map[string]string, len(jsKeys))
			for _, k := range jsKeys {
				m[k] = loc.T(k)
			}
			b, _ := json.Marshal(m) // escapes <, > and & so the block cannot end early
			return template.JS(b)
		},
		"humanSize": func(n int64) string {
			const unit = 1024
			if n < unit {
				return fmt.Sprintf("%d B", n)
			}
			div, exp := int64(unit), 0
			for m := n / unit; m >= unit; m /= unit {
				div *= unit
				exp++
			}
			out := fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
			if lang == "de" {
				out = strings.Replace(out, ".", ",", 1)
			}
			return out
		},
		"date": func(s string) string {
			t, err := database.ParseTime(s)
			if err != nil {
				return s
			}
			return t.Local().Format(layout)
		},
	}
}

var funcs = template.FuncMap{
	"stars": func(n int) []int { // [1..5]
		return []int{1, 2, 3, 4, 5}
	},
	"dict": func(kv ...any) map[string]any {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
	"seq": func(a, b int) []int {
		var out []int
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
		return out
	},
}

func (s *Server) loadTemplates() error {
	shared, err := fs.Glob(web.FS, "templates/partials/*.html")
	if err != nil {
		return err
	}
	pages, err := fs.Glob(web.FS, "templates/pages/*.html")
	if err != nil {
		return err
	}
	s.pages = map[string]map[string]*template.Template{}
	for _, lang := range s.i18n.Languages() {
		fm := localeFuncs(s.i18n.Localizer(lang))
		for k, v := range funcs {
			fm[k] = v
		}
		fm["static"] = s.static.URL
		set := map[string]*template.Template{}
		for _, p := range pages {
			files := append([]string{"templates/base.html"}, shared...)
			files = append(files, p)
			t, err := template.New(path.Base(p)).Funcs(fm).ParseFS(web.FS, files...)
			if err != nil {
				return fmt.Errorf("parse %s: %w", p, err)
			}
			set[strings.TrimSuffix(path.Base(p), ".html")] = t
		}
		s.pages[lang] = set
	}
	return nil
}

// view is the data passed to every full-page template.
type view struct {
	Title   string
	Session *sessions.Session
	CSRF    string
	Data    any
	Lang    string
	Langs   []LangOption
}

// page renders a full page.
func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	loc := s.translator(r)
	t, ok := s.pages[loc.Lang()][name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("unknown page %q", name))
		return
	}
	v := view{Title: title, Data: data, Lang: loc.Lang(), Langs: s.langOptions(r, loc.Lang())}
	if sess := sessionFrom(r.Context()); sess != nil {
		v.Session, v.CSRF = sess, sess.CSRFToken
	}
	b, err := s.execute("base", t, v)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// fragment renders a named partial (any page set contains all partials).
func (s *Server) fragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	t := s.pages[langFrom(r.Context())]["folders"]
	b, err := s.execute(name, t, data)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func now() time.Time { return time.Now() }
