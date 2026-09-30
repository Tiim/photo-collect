package i18n_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/tiim/photo-collect/web"
)

// sources returns the text of every file under root whose name matches ext.
func sources(t *testing.T, root string, skip func(path string) bool, exts ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || skip(path) {
			return err
		}
		for _, e := range exts {
			if strings.HasSuffix(path, e) {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				out[path] = string(b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func catalogIDs(t *testing.T) map[string]any {
	t.Helper()
	data, err := fs.ReadFile(web.FS, "locales/active.en.toml")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := toml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Every message ID used by a template, a script or Go code exists in the
// catalogs, and every catalog entry is used somewhere.
func TestMessageIDsMatchUsage(t *testing.T) {
	ids := catalogIDs(t)
	noTests := func(p string) bool { return strings.HasSuffix(p, "_test.go") || strings.Contains(p, "/leaflet/") }
	files := sources(t, "../../web", noTests, ".html", ".js")
	comment := regexp.MustCompile(`(?m)^\s*//.*$`)
	for p, s := range sources(t, "../../internal", noTests, ".go") {
		files[p] = comment.ReplaceAllString(s, "")
	}
	all := ""
	for _, s := range files {
		all += s + "\n"
	}

	// {{t "id"}} / {{th "id"}} in templates, tr('id', ...) in scripts, and the
	// Go helpers that take an ID as string argument.
	used := map[string]string{}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\{\{-?\s*(?:t|th)\s+"([^"]+)"`),
		regexp.MustCompile(`\(\s*(?:t|th)\s+"([^"]+)"`),
		regexp.MustCompile(`\btr\('([^']+)'`),
		regexp.MustCompile(`\.(?:T|fail)\((?:[^"()]*,\s*)*"([a-z_]+(?:\.[a-z_]+)+)"`),
		regexp.MustCompile(`\bloc\.T\("([^"]+)"`),
		regexp.MustCompile(`\bUserErr\("([^"]+)"`),
		regexp.MustCompile(`\bbad\("([^"]+)"`),
	}
	for p, s := range files {
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				used[m[1]] = p
			}
		}
	}
	// The list of keys the scripts receive.
	for _, m := range regexp.MustCompile(`"((?:js|clock)\.[a-z_]+)"`).FindAllStringSubmatch(files["../../internal/http/templates.go"], -1) {
		used[m[1]] = "internal/http/templates.go (jsKeys)"
	}
	for id, p := range used {
		if _, ok := ids[id]; !ok {
			t.Errorf("message %q used in %s is not in the catalog", id, p)
		}
	}

	var unused []string
	for id := range ids {
		if _, ok := used[id]; ok {
			continue
		}
		// Fallback: the ID appears quoted somewhere (e.g. in a lookup table).
		if strings.Contains(all, `"`+id+`"`) || strings.Contains(all, `'`+id+`'`) {
			continue
		}
		unused = append(unused, id)
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Errorf("catalog entries never used: %v", unused)
	}
}

// Scripts replace {name} markers; a translation must keep them.
func TestScriptPlaceholdersKept(t *testing.T) {
	marker := regexp.MustCompile(`\{[a-z]+\}`)
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
	for id, v := range en {
		s, ok := v.(string)
		if !ok {
			continue
		}
		want := marker.FindAllString(s, -1)
		got, _ := de[id].(string)
		sort.Strings(want)
		g := marker.FindAllString(got, -1)
		sort.Strings(g)
		if strings.Join(want, ",") != strings.Join(g, ",") {
			t.Errorf("%s: markers %v in en, %v in de", id, want, g)
		}
	}
}

// Both plural forms exist wherever English defines them.
func TestPluralFormsComplete(t *testing.T) {
	data, _ := fs.ReadFile(web.FS, "locales/active.de.toml")
	de := map[string]any{}
	if err := toml.Unmarshal(data, &de); err != nil {
		t.Fatal(err)
	}
	for id, v := range catalogIDs(t) {
		en, ok := v.(map[string]any)
		if !ok {
			continue
		}
		got, _ := de[id].(map[string]any)
		for form := range en {
			if _, ok := got[form]; !ok {
				t.Errorf("%s: plural form %q missing in de", id, form)
			}
		}
	}
}
