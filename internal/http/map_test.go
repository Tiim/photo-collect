package http_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"


	"github.com/tiim/photo-collect/internal/images/imagetest"
)

const tileHost = "https://tile.openstreetmap.org"

// directive returns one directive of a Content-Security-Policy header.
func directive(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, name+" ") {
			return d
		}
	}
	return ""
}

// seedGeo seeds folder g1 with a geotagged and an untagged image.
func seedGeo(t *testing.T, e *env) {
	t.Helper()
	seedFolder(t, e, []seed{
		{id: "geo", uploader: "Anna", rating: 4},
		{id: "geo2", uploader: "Ben", rating: 1},
		{id: "nogeo", uploader: "Anna", rating: 5},
	})
	if _, err := e.db.Exec("UPDATE images SET gps_lat = 47.3769, gps_lon = 8.5417 WHERE id = 'geo'"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec("UPDATE images SET gps_lat = -33.8568, gps_lon = 151.2153 WHERE id = 'geo2'"); err != nil {
		t.Fatal(err)
	}
}

func TestImageDetailMap(t *testing.T) {
	e := setup(t)
	seedGeo(t, e)

	rec := e.authed("GET", "/images/geo", nil)
	body := rec.Body.String()
	for _, want := range []string{"47.37690, 8.54170", "openstreetmap.org/?mlat=47.376900&amp;mlon=8.541700"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q:\n%s", want, body)
		}
	}
	// The only map is the folder card: the detail has none and loads no tiles.
	if strings.Contains(body, `class="map`) || strings.Contains(rec.Header().Get("Content-Security-Policy"), tileHost) {
		t.Errorf("image detail shows a map or allows tiles")
	}

	// No position: no location section.
	rec = e.authed("GET", "/images/nogeo", nil)
	if strings.Contains(rec.Body.String(), "Location") {
		t.Errorf("image without GPS shows a location")
	}
}

func TestFolderMapJSON(t *testing.T) {
	e := setup(t)
	seedGeo(t, e)
	points := func(q string) map[string][2]float64 {
		rec := e.authed("GET", "/folders/g1/map.json"+q, nil)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("map.json%s = %d %s", q, rec.Code, rec.Header().Get("Content-Type"))
		}
		var out struct {
			Points []struct {
				ID       string
				Lat, Lon float64
			}
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		m := map[string][2]float64{}
		for _, p := range out.Points {
			m[p.ID] = [2]float64{p.Lat, p.Lon}
		}
		return m
	}
	if got := points(""); len(got) != 2 || got["geo"] != [2]float64{47.3769, 8.5417} || got["geo2"][0] != -33.8568 {
		t.Errorf("all = %v (untagged photos must not appear)", got)
	}
	if got := points("?uploader=Ben"); len(got) != 1 || got["geo2"] == ([2]float64{}) {
		t.Errorf("uploader filter = %v", got)
	}
	if got := points("?rating_min=4"); len(got) != 1 || got["geo"] == ([2]float64{}) {
		t.Errorf("rating filter = %v", got)
	}
	e.db.Exec("UPDATE images SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = 'geo'")
	if got := points(""); len(got) != 1 {
		t.Errorf("trashed photo on the map: %v", got)
	}
	if rec := e.authed("GET", "/folders/g1/map.json?rating_min=x", nil); rec.Code != 400 {
		t.Errorf("bad filter = %d", rec.Code)
	}
	if rec := e.do("GET", "/folders/g1/map.json", nil, nil); rec.Code == 200 {
		t.Errorf("map.json served without a session")
	}
}

// The map card is open whenever a photo matching the filter has a position, sits
// above the panels, and follows the filter (re-rendered out of band).
func TestFolderMapCard(t *testing.T) {
	e := setup(t)
	seedGeo(t, e)
	page := e.authed("GET", "/folders/g1?uploader=Ben", nil).Body.String()
	card := `data-json="/folders/g1/map.json?has_gps=1&amp;uploader=Ben"`
	if !strings.Contains(page, card) {
		t.Fatalf("no map card for a filter with a geotagged match:\n%s", page)
	}
	// A card in the panels row, before the upload link card: not a full-width block.
	if m, p, u := strings.Index(page, `id="map-card"`), strings.Index(page, `class="panels"`), strings.Index(page, "<h2>Upload link</h2>"); !(p < m && m < u) {
		t.Errorf("the map card is not the first card of the folder view (%d %d %d)", p, m, u)
	}
	if !strings.Contains(page, `<div class="panel">
    <h2>Map</h2>`) {
		t.Errorf("map is not rendered as a card")
	}
	// A filter without geotagged matches has no card.
	if body := e.authed("GET", "/folders/g1?uploader=Anna&rating_min=5", nil).Body.String(); strings.Contains(body, "data-json") {
		t.Errorf("map card although no match has a position")
	}
	// Filter changes re-render the card out of band.
	frag := e.authed("GET", "/folders/g1/gallery?uploader=Ben", nil).Body.String()
	if !strings.Contains(frag, `id="map-card" hx-swap-oob="true"`) || !strings.Contains(frag, card) {
		t.Errorf("gallery fragment lacks the OOB map card:\n%s", frag)
	}
	if frag := e.authed("GET", "/folders/g1/gallery?rating_min=5&uploader=Anna", nil).Body.String(); !strings.Contains(frag, `id="map-card" hx-swap-oob="true"`) || strings.Contains(frag, "data-json") {
		t.Errorf("an emptied map card must still be swapped out")
	}
	if rec := e.authed("GET", "/folders/g1/map", nil); rec.Code != 404 {
		t.Errorf("old map panel route = %d", rec.Code)
	}
}

// No CSP anywhere may allow a third-party script or style host, and the tile
// host is allowed for images only, on map pages only.
func TestCSPNeverAllowsThirdPartyCode(t *testing.T) {
	e := setup(t)
	seedGeo(t, e)
	token, _ := e.newLink("Guests")
	for _, p := range []string{"/folders", "/folders/g1", "/images/geo", "/images/nogeo", "/upload/" + token, "/", "/folders/g1/trash", "/folders/g1/gallery"} {
		rec := e.authed("GET", p, nil)
		csp := rec.Header().Get("Content-Security-Policy")
		for _, d := range []string{"default-src", "script-src", "style-src", "connect-src", "font-src"} {
			if strings.Contains(directive(csp, d), "http") {
				t.Errorf("%s: %s allows an external host: %q", p, d, directive(csp, d))
			}
		}
		if img := directive(csp, "img-src"); strings.Contains(img, "http") && img != "img-src 'self' data: "+tileHost {
			t.Errorf("%s: img-src = %q", p, img)
		}
		if strings.Contains(csp, tileHost) && p != "/folders/g1" {
			t.Errorf("%s allows the tile host but shows no map", p)
		}
	}
}

// Leaflet is vendored, and the pages reference no third-party script or stylesheet.
func TestLeafletIsVendored(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/static/map.js", "/static/leaflet/leaflet.js", "/static/leaflet/leaflet.css", "/static/leaflet/leaflet.markercluster.js",
		"/static/leaflet/images/marker-icon.png", "/static/leaflet/VERSION", "/static/leaflet/LICENSE-leaflet"} {
		if rec := e.do("GET", p, nil, nil); rec.Code != 200 {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
	seedGeo(t, e)
	ext := regexp.MustCompile(`(?:src|href)="https?://`)
	for _, p := range []string{"/folders/g1", "/images/geo"} {
		body := e.do("GET", p, nil, nil, e.cookie).Body.String()
		for _, m := range ext.FindAllString(body, -1) {
			// Only plain links (openstreetmap.org, ...) are fine, never <script> or <link>.
			t.Logf("%s: %s", p, m)
		}
		if regexp.MustCompile(`<(script|link)[^>]+(src|href)="https?://`).MatchString(body) {
			t.Errorf("%s loads an external script or stylesheet", p)
		}
	}
}

// An uploaded photo's position is stored, but derivatives carry no EXIF, so
// nothing leaks through preview or thumbnail.
func TestUploadStoresGPSAndDerivativesHaveNoEXIF(t *testing.T) {
	e := setup(t)
	token, folderPath := e.newLink("Trip")
	rec := e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Alice"), form)
	nick := rec.Result().Cookies()[0]
	data := imagetest.JPEG(80, 60, imagetest.Camera{Make: "Apple", Model: "iPhone", HasGPS: true, Lat: 47.3769, Lon: 8.5417})
	if code, res := e.upload(token, nick, "geo.jpg", data); code != 200 || res[0]["ok"] != true {
		t.Fatalf("upload: %d %v", code, res)
	}
	folderID := strings.TrimPrefix(folderPath, "/folders/")
	var id string
	var lat, lon float64
	if err := e.db.QueryRow("SELECT id, gps_lat, gps_lon FROM images WHERE folder_id = ?", folderID).Scan(&id, &lat, &lon); err != nil {
		t.Fatal(err)
	}
	if lat < 47.376 || lat > 47.378 || lon < 8.541 || lon > 8.543 {
		t.Errorf("stored position %v, %v", lat, lon)
	}
	if n := e.scalar("SELECT COUNT(*) FROM images WHERE gps_attempted_at IS NOT NULL"); n != 1 {
		t.Errorf("gps_attempted_at not set at ingest")
	}
	waitFor(t, "derivatives", func() bool {
		return e.scalar("SELECT COUNT(*) FROM images WHERE thumbnail_ready = 1 AND preview_ready = 1") == 1
	})
	orig, err := os.ReadFile(filepath.Join(e.storeDir, "folders", folderID, "images", id, "original"))
	if err != nil || !bytes.Contains(orig, []byte("Exif")) {
		t.Fatalf("original lost its EXIF (%v)", err)
	}
	for _, kind := range []string{"preview", "thumbnail"} {
		b, err := os.ReadFile(filepath.Join(e.storeDir, "folders", folderID, "images", id, kind))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("Exif")) || bytes.Contains(b, []byte("GPS")) || bytes.Contains(b, []byte("Apple")) {
			t.Errorf("%s carries metadata", kind)
		}
	}
}
