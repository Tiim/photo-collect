package http_test

import (
	"regexp"
	"strings"
	"testing"
)

func TestStaticAssetsAreVersioned(t *testing.T) {
	e := setup(t)
	token, _ := e.newLink("A")
	page := e.do("GET", "/upload/"+token, nil, nil).Body.String()
	m := regexp.MustCompile(`src="/static/app\.js\?v=([0-9a-f]{12})"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("app.js not linked with a version:\n%s", page)
	}
	for _, want := range []string{`href="/static/style.css?v=`, `src="/static/htmx.min.js?v=`, `href="/static/favicon.svg?v=`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if clock := e.do("GET", "/", nil, nil).Body.String(); !strings.Contains(clock, `src="/static/clock.js?v=`) || !strings.Contains(clock, `src="/static/qrcode.js?v=`) {
		t.Errorf("clock page scripts not versioned:\n%s", clock)
	}
	if folders := e.authed("GET", "/folders", nil).Body.String(); !strings.Contains(folders, `src="/static/map.js?v=`) {
		t.Errorf("map.js not versioned")
	}

	// The current version may be cached for good.
	rec := e.do("GET", "/static/app.js?v="+m[1], nil, nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || rec.Header().Get("ETag") != `"`+m[1]+`"` {
		t.Fatalf("versioned: %d %v", rec.Code, rec.Header())
	}
	// Unversioned and outdated URLs must be revalidated.
	for _, target := range []string{"/static/app.js", "/static/app.js?v=000000000000", "/static/leaflet/leaflet.js"} {
		rec := e.do("GET", target, nil, nil)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") == "" {
			t.Errorf("%s: %d %v", target, rec.Code, rec.Header())
		}
	}
	// Revalidating an unchanged file costs no body.
	rec = e.do("GET", "/static/app.js", nil, map[string]string{"If-None-Match": `"` + m[1] + `"`})
	if rec.Code != 304 || rec.Body.Len() != 0 {
		t.Errorf("revalidation: %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	if rec := e.do("GET", "/static/nope.js", nil, nil); rec.Code != 404 {
		t.Errorf("missing file: %d", rec.Code)
	}
}
