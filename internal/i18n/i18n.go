// Package i18n is a thin wrapper around go-i18n: it loads the embedded TOML
// catalogs, resolves the language of a request and returns per-request
// translators.
package i18n

import (
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

// Default is the language used when nothing else matches and the fallback for
// missing messages.
const Default = "en"

// Bundle holds all loaded catalogs.
type Bundle struct {
	bundle  *goi18n.Bundle
	matcher language.Matcher
	langs   []string // supported language codes, default first
	log     *slog.Logger
	warned  sync.Map // message IDs already reported as missing
}

// New loads every active.<lang>.toml file in dir of fsys. The default language
// must be present.
func New(fsys fs.FS, dir string, log *slog.Logger) (*Bundle, error) {
	if log == nil {
		log = slog.Default()
	}
	b := &Bundle{bundle: goi18n.NewBundle(language.English), log: log}
	b.bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var tags []language.Tag
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "active.") || !strings.HasSuffix(name, ".toml") {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, err
		}
		mf, err := b.bundle.ParseMessageFileBytes(data, name)
		if err != nil {
			return nil, fmt.Errorf("i18n: %s: %w", name, err)
		}
		tags = append(tags, mf.Tag)
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("i18n: no catalogs in %s", dir)
	}
	// Default first: the matcher falls back to the first tag.
	def := language.Make(Default)
	ordered := []language.Tag{def}
	for _, t := range tags {
		if t != def {
			ordered = append(ordered, t)
		}
	}
	if len(ordered) != len(tags) {
		return nil, fmt.Errorf("i18n: catalog for default language %q missing", Default)
	}
	for _, t := range ordered {
		b.langs = append(b.langs, t.String())
	}
	b.matcher = language.NewMatcher(ordered)
	return b, nil
}

// Languages returns the supported language codes, default first.
func (b *Bundle) Languages() []string { return append([]string(nil), b.langs...) }

// Supported reports whether code is exactly a supported language.
func (b *Bundle) Supported(code string) bool {
	for _, l := range b.langs {
		if l == code {
			return true
		}
	}
	return false
}

// Match picks the supported language for an Accept-Language header value.
func (b *Bundle) Match(acceptLanguage string) string {
	tags, _, err := language.ParseAcceptLanguage(acceptLanguage)
	if err != nil || len(tags) == 0 {
		return Default
	}
	_, idx, conf := b.matcher.Match(tags...)
	if conf == language.No {
		return Default
	}
	return b.langs[idx]
}

// Localizer translates messages into one language, falling back to the default
// language and then to the message ID.
type Localizer struct {
	b    *Bundle
	lang string
	loc  *goi18n.Localizer
}

// Localizer returns a translator for lang.
func (b *Bundle) Localizer(lang string) *Localizer {
	return &Localizer{b: b, lang: lang, loc: goi18n.NewLocalizer(b.bundle, lang, Default)}
}

// Lang returns the language code this localizer was created for.
func (l *Localizer) Lang() string { return l.lang }

// T returns the message id. The optional data is the template data of the
// message; when it is a map with a "Count" key, Count also selects the plural form.
func (l *Localizer) T(id string, data ...any) string {
	cfg := &goi18n.LocalizeConfig{MessageID: id}
	if len(data) > 0 {
		cfg.TemplateData = data[0]
		if m, ok := data[0].(map[string]any); ok {
			if c, ok := m["Count"]; ok {
				cfg.PluralCount = c
			}
		}
	}
	s, err := l.loc.Localize(cfg)
	if err != nil {
		if _, seen := l.b.warned.LoadOrStore(id, true); !seen {
			l.b.log.Debug("i18n: message not found", "id", id, "lang", l.lang, "err", err)
		}
		if s == "" {
			return id
		}
	}
	return s
}
