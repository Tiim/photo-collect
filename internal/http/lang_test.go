package http_test

import (
	"context"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestLanguageResolution(t *testing.T) {
	e := setup(t)
	rec := e.authed("POST", "/folders", url.Values{"name": {"Lang"}})
	folderPath := rec.Header().Get("HX-Redirect")
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	m := regexp.MustCompile(`/upload/([A-Za-z0-9_-]+)"`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("create link: %d", rec.Code)
	}
	page := "/upload/" + m[1]

	get := func(target, accept, cookie string) (string, string) {
		hdr := map[string]string{}
		if accept != "" {
			hdr["Accept-Language"] = accept
		}
		if cookie != "" {
			hdr["Cookie"] = "lang=" + cookie
		}
		rec := e.do("GET", target, nil, hdr)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d", target, rec.Code)
		}
		return rec.Body.String(), rec.Header().Get("Set-Cookie")
	}
	tests := []struct {
		name, target, accept, cookie, wantHTML, wantLang string
	}{
		{"default", page, "", "", "Your nickname", "en"},
		{"accept-language", page, "de-CH,de;q=0.9,en;q=0.5", "", "Dein Spitzname", "de"},
		{"unsupported accept-language", page, "fr-FR", "", "Your nickname", "en"},
		{"cookie beats header", page, "de", "en", "Your nickname", "en"},
		{"query beats cookie", page + "?lang=de", "", "en", "Dein Spitzname", "de"},
		{"invalid query ignored", page + "?lang=xx", "", "de", "Dein Spitzname", "de"},
		{"invalid cookie ignored", page, "", "xx", "Your nickname", "en"},
	}
	for _, tt := range tests {
		body, _ := get(tt.target, tt.accept, tt.cookie)
		if !strings.Contains(body, tt.wantHTML) || !strings.Contains(body, `<html lang="`+tt.wantLang+`">`) {
			t.Errorf("%s: want %q / lang %s in:\n%s", tt.name, tt.wantHTML, tt.wantLang, body)
		}
	}

	_, cookie := get(page+"?lang=de", "", "")
	for _, want := range []string{"lang=de", "SameSite=Lax", "Max-Age=31536000", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("Set-Cookie %q missing %q", cookie, want)
		}
	}
	if _, cookie := get(page, "de", ""); cookie != "" {
		t.Errorf("cookie set without ?lang=: %q", cookie)
	}

	// Switcher keeps the rest of the query and marks the current language.
	body, _ := get(page+"?x=1", "", "")
	if !strings.Contains(body, `href="/upload/`+m[1]+`?lang=de&amp;x=1"`) || !strings.Contains(body, "<strong lang=\"en\">English</strong>") {
		t.Errorf("switcher missing:\n%s", body)
	}
}

// TestGermanUI renders the signed-in pages and fragments in German: no message
// ID may leak into the output, errors and formats follow the language.
func TestGermanUI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f, err := e.db.Q.CreateFolder(ctx, sqlc.CreateFolderParams{ID: "f1", Name: "Fest"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Q.InsertImage(ctx, sqlc.InsertImageParams{
		ID: "i1", FolderID: f.ID, OriginalFilename: "a.jpg", MimeType: "image/jpeg", SizeBytes: 1536, Width: 1, Height: 1,
		Sha256: strings.Repeat("a", 64), UploaderNickname: "Anna",
	}); err != nil {
		t.Fatal(err)
	}
	de := map[string]string{"Accept-Language": "de", "HX-Request": "true"}
	get := func(target string) string {
		rec := e.do("GET", target, nil, de, e.cookie)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d", target, rec.Code)
		}
		return rec.Body.String()
	}
	leak := regexp.MustCompile(`\b(?:nav|upload|clock|folders?|trash|dup|exports|filter|gallery|detail|tags|tile|map|rating|link|js|err|device)\.[a-z_]+\b`)
	block := regexp.MustCompile(`(?s)<script type="application/json" id="i18n">.*?</script>`)
	static := regexp.MustCompile(`/static/[\w./-]+`)
	for _, p := range []string{"/folders", "/folders/f1", "/folders/f1/trash", "/folders/f1/downloads", "/images/i1", "/images/i1/tile", "/"} {
		body := block.ReplaceAllString(static.ReplaceAllString(get(p), ""), "")
		if m := leak.FindString(body); m != "" {
			t.Errorf("GET %s: message ID %q shown instead of text", p, m)
		}
	}
	if body := get("/folders"); !strings.Contains(body, "Ordner erstellen") || !strings.Contains(body, "1 Bild ·") {
		t.Errorf("folders page not German:\n%s", body)
	}
	if body := get("/images/i1"); !strings.Contains(body, "1,5 KiB") || !strings.Contains(body, "Hochgeladen") {
		t.Errorf("image detail lacks German text or decimal comma:\n%s", body)
	}
	if body := get("/"); !strings.Contains(body, "Kamerauhr prüfen") || !strings.Contains(body, `"js.copied":"Kopiert"`) {
		t.Errorf("clock page not German or lacks script strings:\n%s", body)
	}
	// English pages carry the script strings too.
	if body := e.do("GET", "/folders", nil, nil, e.cookie).Body.String(); !strings.Contains(body, `"js.copied":"Copied"`) {
		t.Errorf("script strings missing:\n%s", body)
	}

	// Errors are translated at the edge.
	rec := e.do("POST", "/images/i1/tags", strings.NewReader(url.Values{"name": {""}}.Encode()),
		map[string]string{"X-CSRF-Token": e.csrf, "Accept-Language": "de", "Content-Type": "application/x-www-form-urlencoded"}, e.cookie)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Der Tag ist leer") {
		t.Errorf("tag error: %d %q", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/folders/f1?rating_min=9", nil, de, e.cookie); rec.Code != 400 || !strings.Contains(rec.Body.String(), "zwischen 1 und 5") {
		t.Errorf("filter error: %d %q", rec.Code, rec.Body)
	}
}
