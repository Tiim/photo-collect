package http_test

import (
	"archive/zip"
	"bytes"
	nethttp "net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// share adds images of te's folder to a share (a new one for shareID "") and
// returns the share's ID.
func (te *trashEnv) share(shareID string, ids ...string) string {
	te.t.Helper()
	rec := te.authed("POST", te.folderPath+"/shares", url.Values{"share": {shareID}, "image": ids, "csrf": {te.csrf}})
	loc := rec.Header().Get("HX-Redirect")
	if !strings.HasPrefix(loc, "/shares/") {
		te.t.Fatalf("share: %d %q %s", rec.Code, loc, rec.Body)
	}
	return strings.TrimPrefix(loc, "/shares/")
}

// shareToken reads the public link from the share's management page.
func (te *trashEnv) shareToken(shareID string) string {
	te.t.Helper()
	rec := te.authed("GET", "/shares/"+shareID, nil)
	if rec.Code != nethttp.StatusOK {
		te.t.Fatalf("share page: %d", rec.Code)
	}
	return regexpMatch(te.t, rec.Body.String(), `http://example\.test/s/([A-Za-z0-9_-]+)"`)
}

// anon requests target without a session.
func (e *env) anon(target string) *nethttp.Response {
	return e.do("GET", target, nil, nil).Result()
}

func body(r *nethttp.Response) string {
	var b bytes.Buffer
	b.ReadFrom(r.Body)
	return b.String()
}

func TestSharePublicGallery(t *testing.T) {
	te := newFolderWithImages(setup(t), "Summer party", [2]int{60, 40}, [2]int{50, 50}, [2]int{40, 60})
	a, b, c := te.ids[0], te.ids[1], te.ids[2]
	sid := te.share("", a, b)
	tok := te.shareToken(sid)

	// The folder page lists the share.
	if page := te.authed("GET", te.folderPath, nil).Body.String(); !strings.Contains(page, `/shares/`+sid) || !strings.Contains(page, "Summer party") {
		t.Error("folder page does not link the share")
	}

	res := te.anon("/s/" + tok)
	html := body(res)
	if res.StatusCode != 200 {
		t.Fatalf("public page: %d %s", res.StatusCode, html)
	}
	if res.Header.Get("X-Robots-Tag") == "" {
		t.Error("public page may be indexed")
	}
	for _, id := range []string{a, b} {
		if !strings.Contains(html, "/s/"+tok+"/i/"+id+"/thumbnail") {
			t.Errorf("shared image %s missing", id)
		}
	}
	if strings.Contains(html, c) {
		t.Error("unshared image is visible")
	}
	if strings.Contains(html, "Alice") {
		t.Error("public page shows the uploader's nickname")
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/i/" + a, 200},
		{"/i/" + a + "/thumbnail", 200},
		{"/i/" + a + "/preview", 200},
		{"/i/" + a + "/original", 200},
		{"/i/" + a + "/bogus", 404},
		{"/i/" + c, 404},
		{"/i/" + c + "/original", 404},
	} {
		if got := te.anon("/s/" + tok + tc.path).StatusCode; got != tc.want {
			t.Errorf("%s: %d, want %d", tc.path, got, tc.want)
		}
	}
	if !strings.Contains(body(te.anon("/s/"+tok+"/i/"+a)), "/s/"+tok+"/i/"+b) {
		t.Error("image view does not link the next shared image")
	}
	// Signed-in routes stay closed.
	if got := te.anon("/images/" + a + "/original").StatusCode; got != nethttp.StatusFound {
		t.Errorf("/images without session: %d", got)
	}
	if got := te.anon("/shares/" + sid).StatusCode; got != nethttp.StatusFound {
		t.Errorf("/shares without session: %d", got)
	}
	if got := te.anon("/s/not-a-token").StatusCode; got != 404 {
		t.Errorf("unknown token: %d", got)
	}

	// ZIP of the originals, without sidecars.
	res = te.anon("/s/" + tok + "/download.zip")
	data := []byte(body(res))
	if res.StatusCode != 200 || res.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("zip: %d, length %s vs %d", res.StatusCode, res.Header.Get("Content-Length"), len(data))
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Summer party/undated/pa.jpg,Summer party/undated/pb.jpg" {
		t.Errorf("zip entries: %v", names)
	}
}

func TestShareEdit(t *testing.T) {
	te := newFolderWithImages(setup(t), "Party", [2]int{60, 40}, [2]int{50, 50}, [2]int{40, 60})
	a, b, c := te.ids[0], te.ids[1], te.ids[2]
	sid := te.share("", a)
	tok := te.shareToken(sid)
	visible := func(id string) bool { return te.anon("/s/"+tok+"/i/"+id).StatusCode == 200 }

	// Add to the existing share; adding twice is harmless.
	if got := te.share(sid, b, c, a); got != sid {
		t.Fatalf("added to %s, want %s", got, sid)
	}
	if !visible(a) || !visible(b) || !visible(c) {
		t.Fatal("added images not visible")
	}
	if n := te.scalar("SELECT COUNT(*) FROM shares"); n != 1 {
		t.Errorf("%d shares, want 1", n)
	}

	// Remove from the share: the image stays in the folder.
	rec := te.authed("POST", "/shares/"+sid+"/images/remove", url.Values{"image": {a}})
	if rec.Header().Get("HX-Redirect") != "/shares/"+sid {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if visible(a) {
		t.Error("removed image still visible")
	}
	if n := te.scalar("SELECT COUNT(*) FROM images WHERE id = ? AND deleted_at IS NULL", a); n != 1 {
		t.Error("removing from the share touched the image")
	}

	// Trashed images disappear from the share.
	te.authed("POST", te.folderPath+"/images/trash", url.Values{"image": {b}})
	if visible(b) || strings.Contains(body(te.anon("/s/"+tok)), b) {
		t.Error("trashed image still shared")
	}

	// Rename.
	te.authed("POST", "/shares/"+sid+"/rename", url.Values{"name": {"  For   Grandma "}})
	if !strings.Contains(body(te.anon("/s/"+tok)), "For Grandma") {
		t.Error("rename not shown")
	}
	if rec := te.authed("POST", "/shares/"+sid+"/rename", url.Values{"name": {" "}}); rec.Code != 400 {
		t.Errorf("empty name: %d", rec.Code)
	}

	// A share of another folder cannot receive this folder's images.
	other := newFolderWithImages(te.env, "Other", [2]int{30, 30})
	if rec := other.authed("POST", other.folderPath+"/shares", url.Values{"share": {sid}, "image": other.ids}); rec.Code != 404 {
		t.Errorf("cross-folder add: %d", rec.Code)
	}
	if visible(other.ids[0]) {
		t.Error("image of another folder got shared")
	}

	// Without a selection nothing is created.
	if rec := te.authed("POST", te.folderPath+"/shares", url.Values{}); rec.Code != 400 {
		t.Errorf("empty selection: %d", rec.Code)
	}
}

func TestShareExpiryAndDelete(t *testing.T) {
	te := newFolderWithImages(setup(t), "Party", [2]int{60, 40})
	a := te.ids[0]
	sid := te.share("", a)
	tok := te.shareToken(sid)

	if _, err := te.db.Exec("UPDATE shares SET expires_at = '2000-01-01T00:00:00.000Z'"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "/i/" + a, "/i/" + a + "/original", "/download.zip"} {
		if got := te.anon("/s/" + tok + p).StatusCode; got != nethttp.StatusGone {
			t.Errorf("expired %q: %d", p, got)
		}
	}
	if !strings.Contains(te.authed("GET", te.folderPath, nil).Body.String(), "Expired") {
		t.Error("folder page does not mark the share as expired")
	}

	// Extending keeps the link.
	te.authed("POST", "/shares/"+sid+"/extend", url.Values{"days": {"3"}})
	if got := te.anon("/s/" + tok).StatusCode; got != 200 {
		t.Errorf("after extend: %d", got)
	}
	if te.shareToken(sid) != tok {
		t.Error("extend changed the token")
	}

	rec := te.authed("POST", "/shares/"+sid+"/delete", nil)
	if rec.Header().Get("HX-Redirect") != te.folderPath {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if got := te.anon("/s/" + tok).StatusCode; got != 404 {
		t.Errorf("deleted share: %d", got)
	}

	// Deleting the folder ends its shares.
	sid = te.share("", a)
	tok = te.shareToken(sid)
	te.authed("POST", te.folderPath+"/delete", nil)
	if got := te.anon("/s/" + tok).StatusCode; got != 404 {
		t.Errorf("share of deleted folder: %d", got)
	}
}

func TestShareLabelIsPrivate(t *testing.T) {
	te := newFolderWithImages(setup(t), "Party", [2]int{60, 40}, [2]int{50, 50})
	rec := te.authed("POST", te.folderPath+"/shares", url.Values{"image": {te.ids[0]}, "label": {"  Grandma  "}})
	sid := strings.TrimPrefix(rec.Header().Get("HX-Redirect"), "/shares/")
	te.share("", te.ids[1]) // a second share without a name
	tok := te.shareToken(sid)

	if n := te.scalar("SELECT COUNT(*) FROM shares WHERE label = 'Grandma'"); n != 1 {
		t.Fatalf("label not stored")
	}
	folder := te.authed("GET", te.folderPath, nil).Body.String()
	if !strings.Contains(folder, ">Grandma</a>") || !strings.Contains(folder, "Add to “Grandma”") {
		t.Error("folder page does not show the share's name")
	}
	if !strings.Contains(te.authed("GET", "/shares/"+sid, nil).Body.String(), "<h1>Grandma</h1>") {
		t.Error("share page does not show the name")
	}
	if pub := body(te.anon("/s/" + tok)); strings.Contains(pub, "Grandma") || !strings.Contains(pub, "<h1>Party</h1>") {
		t.Error("public page shows the private name or lacks the title")
	}

	te.authed("POST", "/shares/"+sid+"/rename", url.Values{"name": {"Photos for you"}, "label": {"Grandpa"}})
	if n := te.scalar("SELECT COUNT(*) FROM shares WHERE label = 'Grandpa' AND name = 'Photos for you'"); n != 1 {
		t.Error("rename did not update name and title")
	}
	if rec := te.authed("POST", "/shares/"+sid+"/rename", url.Values{"name": {"x"}, "label": {strings.Repeat("a", 101)}}); rec.Code != 400 {
		t.Errorf("long name: %d", rec.Code)
	}
}
