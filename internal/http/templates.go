package http

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/sessions"
	"github.com/tiim/photo-collect/web"
)

var funcs = template.FuncMap{
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
		return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
	},
	"date": func(s string) string {
		t, err := database.ParseTime(s)
		if err != nil {
			return s
		}
		return t.Local().Format("2006-01-02 15:04")
	},
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
	s.pages = map[string]*template.Template{}
	for _, p := range pages {
		files := append([]string{"templates/base.html"}, shared...)
		files = append(files, p)
		t, err := template.New(path.Base(p)).Funcs(funcs).ParseFS(web.FS, files...)
		if err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		s.pages[strings.TrimSuffix(path.Base(p), ".html")] = t
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
	// T translates a message ID: {{call $.T "id"}} or {{call $.T "id" (dict "Name" .X)}}.
	T func(id string, data ...any) string
	// TH is T for messages containing markup; string arguments are escaped.
	TH func(id string, data ...any) template.HTML
}

// page renders a full page.
func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("unknown page %q", name))
		return
	}
	loc := s.translator(r)
	v := view{
		Title: title, Data: data, Lang: loc.Lang(), Langs: s.langOptions(r, loc.Lang()), T: loc.T,
		TH: func(id string, d ...any) template.HTML { return tHTML(loc, id, d...) },
	}
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
	t := s.pages["folders"]
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
