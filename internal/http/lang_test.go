package http_test

import (
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
