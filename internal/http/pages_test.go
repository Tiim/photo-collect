package http_test

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/tiim/photo-collect/internal/database/sqlc"
)

// TestPagesRender smoke-tests every HTML page and fragment.
func TestPagesRender(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f, err := e.db.Q.CreateFolder(ctx, sqlc.CreateFolderParams{ID: "f1", Name: "<b>Tricky & name</b>"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := e.db.Q.InsertImage(ctx, sqlc.InsertImageParams{
		ID: "i1", FolderID: f.ID, OriginalFilename: `a"b<c>.jpg`, MimeType: "image/jpeg", SizeBytes: 10, Width: 1, Height: 1,
		Sha256: strings.Repeat("a", 64), UploaderNickname: "<script>alert(1)</script>",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.db.Q.SetImageRating(ctx, sqlc.SetImageRatingParams{Rating: sqlNullInt(3), ID: img.ID})

	for _, p := range []string{"/folders", "/folders/f1", "/folders/f1/images", "/folders/f1/downloads", "/images/i1", "/images/i1/tile", "/tags/suggest?name=g"} {
		rec := e.authed("GET", p, nil)
		if rec.Code != 200 {
			t.Errorf("GET %s: %d %s", p, rec.Code, rec.Body)
			continue
		}
		if strings.Contains(rec.Body.String(), "<script>alert") || strings.Contains(rec.Body.String(), "<b>Tricky") {
			t.Errorf("GET %s: unescaped user content:\n%s", p, rec.Body)
		}
	}
	// Full-page variants (non-htmx) of the image page.
	rec := e.do("GET", "/images/i1", nil, nil, e.cookie)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("full image page: %d", rec.Code)
	}
	if rec := e.authed("POST", "/images/i1/rating", url.Values{"rating": {"0"}}); rec.Code != 200 || strings.Contains(rec.Body.String(), `class="star on"`) {
		t.Errorf("clear rating: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/upload/nope", nil, nil); rec.Code != 404 {
		t.Errorf("unknown token: %d", rec.Code)
	}
	if rec := e.do("GET", "/healthz", nil, nil); rec.Code != 200 {
		t.Errorf("healthz: %d", rec.Code)
	} else if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
		// "no-referrer" makes browsers send "Origin: null" on form posts, breaking the CSRF origin check.
		t.Errorf("Referrer-Policy = %q, want same-origin", got)
	}
	if rec := e.do("GET", "/readyz", nil, nil); rec.Code != 200 {
		t.Errorf("readyz: %d", rec.Code)
	}
	if rec := e.do("GET", "/static/app.js", nil, nil); rec.Code != 200 {
		t.Errorf("static: %d", rec.Code)
	}
}

func sqlNullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func sqlNullStr(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
