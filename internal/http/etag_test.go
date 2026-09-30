package http_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestDerivativeETag(t *testing.T) {
	e := setup(t)
	rec := e.authed("POST", "/folders", url.Values{"name": {"E"}})
	folderPath := rec.Header().Get("HX-Redirect")
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	token := regexpMatch(t, rec.Body.String(), `/upload/([A-Za-z0-9_-]+)"`)
	rec = e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Alice"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	nick := rec.Result().Cookies()[0]
	if code, res := e.upload(token, nick, "a.jpg", jpegFile(t, 100, 60)); code != 200 || res[0]["ok"] != true {
		t.Fatalf("upload: %d %v", code, res)
	}
	var id string
	waitFor(t, "thumbnail", func() bool {
		return e.db.QueryRow("SELECT id FROM images WHERE thumbnail_ready=1 AND phash_attempted_at IS NOT NULL").Scan(&id) == nil
	})

	get := func(path, inm string) (int, string) {
		hdr := map[string]string{}
		if inm != "" {
			hdr["If-None-Match"] = inm
		}
		r := e.do("GET", path, nil, hdr, e.cookie)
		return r.Code, r.Header().Get("ETag")
	}
	code, etag1 := get("/images/"+id+"/thumbnail", "")
	if code != 200 || !strings.HasSuffix(etag1, `-thumbnail-2"`) {
		t.Fatalf("thumbnail: %d %q", code, etag1)
	}
	for _, inm := range []string{etag1, "W/" + etag1, `"nope", ` + etag1, "*"} {
		if code, _ := get("/images/"+id+"/thumbnail", inm); code != 304 {
			t.Errorf("If-None-Match %q: code %d, want 304", inm, code)
		}
	}
	if _, oe := get("/images/"+id+"/original", ""); strings.Count(oe, "-") != 1 {
		t.Errorf("original ETag %q should stay unversioned", oe)
	}

	// Re-derivation changes the ETag, so the old validator no longer matches.
	if err := e.db.Q.BumpDerivativeVersion(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	code, etag2 := get("/images/"+id+"/thumbnail", etag1)
	if code != 200 || etag2 == etag1 {
		t.Fatalf("after re-derive: %d, etag %q vs %q", code, etag2, etag1)
	}
}
