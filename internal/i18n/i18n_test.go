package i18n_test

import (
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/BurntSushi/toml"
	"github.com/tiim/photo-collect/internal/i18n"
	"github.com/tiim/photo-collect/web"
)

func testBundle(t *testing.T) *i18n.Bundle {
	fsys := fstest.MapFS{
		"active.en.toml": {Data: []byte(`
hello = "Hello {{.Name}}"
only_en = "Only English"
[photos]
one = "{{.Count}} photo"
other = "{{.Count}} photos"
`)},
		"active.de.toml": {Data: []byte(`
hello = "Hallo {{.Name}}"
[photos]
one = "{{.Count}} Foto"
other = "{{.Count}} Fotos"
`)},
	}
	b, err := i18n.New(fsys, ".", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTranslateFallbackAndPlural(t *testing.T) {
	b := testBundle(t)
	de := b.Localizer("de")
	if got := de.T("hello", map[string]any{"Name": "Anna"}); got != "Hallo Anna" {
		t.Errorf("de hello = %q", got)
	}
	if got := de.T("only_en"); got != "Only English" {
		t.Errorf("fallback to English = %q", got)
	}
	if got := de.T("does.not.exist"); got != "does.not.exist" {
		t.Errorf("fallback to ID = %q", got)
	}
	if got := de.T("photos", map[string]any{"Count": 1}); got != "1 Foto" {
		t.Errorf("one = %q", got)
	}
	if got := b.Localizer("en").T("photos", map[string]any{"Count": 3}); got != "3 photos" {
		t.Errorf("other = %q", got)
	}
}

func TestMatch(t *testing.T) {
	b := testBundle(t)
	for accept, want := range map[string]string{
		"": "en", "de": "de", "de-CH,de;q=0.9": "de", "fr": "en", "en-GB": "en", "garbage;;": "en",
		"fr,de;q=0.5": "de",
	} {
		if got := b.Match(accept); got != want {
			t.Errorf("Match(%q) = %q, want %q", accept, got, want)
		}
	}
	if !b.Supported("de") || b.Supported("fr") || b.Supported("") {
		t.Error("Supported wrong")
	}
	if l := b.Languages(); len(l) != 2 || l[0] != "en" {
		t.Errorf("Languages = %v", l)
	}
}

func TestNewRequiresDefault(t *testing.T) {
	_, err := i18n.New(fstest.MapFS{"active.de.toml": {Data: []byte(`a = "b"`)}}, ".", nil)
	if err == nil {
		t.Error("expected error without English catalog")
	}
}

// The shipped catalogs must define the same IDs with the same placeholders.
func TestShippedCatalogsInSync(t *testing.T) {
	load := func(lang string) map[string]any {
		data, err := fs.ReadFile(web.FS, "locales/active."+lang+".toml")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if err := toml.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	en, de := load("en"), load("de")
	ph := regexp.MustCompile(`\{\{\s*\.(\w+)\s*\}\}`)
	placeholders := func(v any) string {
		var names []string
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case string:
				for _, m := range ph.FindAllStringSubmatch(x, -1) {
					names = append(names, m[1])
				}
			case map[string]any:
				for _, c := range x {
					walk(c)
				}
			}
		}
		walk(v)
		sort.Strings(names)
		out := ""
		for i, n := range names {
			if i == 0 || n != names[i-1] {
				out += n + ","
			}
		}
		return out
	}
	for id, v := range en {
		w, ok := de[id]
		if !ok {
			t.Errorf("%s missing in de", id)
			continue
		}
		if placeholders(v) != placeholders(w) {
			t.Errorf("%s: placeholders differ: %q vs %q", id, placeholders(v), placeholders(w))
		}
	}
	for id := range de {
		if _, ok := en[id]; !ok {
			t.Errorf("%s missing in en", id)
		}
	}
}
