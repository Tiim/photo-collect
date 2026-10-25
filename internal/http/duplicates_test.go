package http_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/tiim/photo-collect/internal/config"
)

// TestDuplicateDetectionEndToEnd exercises the full flow: an exact
// byte-identical re-upload is silently skipped, a near-duplicate (same
// picture, slightly different size) is flagged for manual review once its
// perceptual hash is computed, and an admin can resolve the pair by keeping
// one and merging tags/rating onto it, or dismiss it as not a duplicate.
func TestDuplicateDetectionEndToEnd(t *testing.T) {
	e := setup(t)

	rec := e.authed("POST", "/folders", url.Values{"name": {"Reunion"}})
	folderPath := rec.Header().Get("HX-Redirect")
	folderID := strings.TrimPrefix(folderPath, "/folders/")
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	token := regexpMatch(t, rec.Body.String(), `/upload/([A-Za-z0-9_-]+)"`)

	rec = e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Alice"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	nick := rec.Result().Cookies()[0]

	// Exact re-upload: same bytes, different filename, is skipped rather
	// than creating a second image.
	code, res := e.upload(token, nick, "photo.jpg", jpegFile(t, 100, 60))
	if code != 200 || res[0]["ok"] != true {
		t.Fatalf("first upload: %d %v", code, res)
	}
	code, res = e.upload(token, nick, "photo-copy.jpg", jpegFile(t, 100, 60))
	if code != 200 || res[0]["ok"] != true {
		t.Fatalf("exact duplicate upload: %d %v", code, res)
	}
	imgs, err := e.db.Q.CountImagesInFolder(context.Background(), folderID)
	if err != nil {
		t.Fatal(err)
	}
	if imgs != 1 {
		t.Fatalf("images after exact duplicate = %d, want 1 (skipped)", imgs)
	}

	// A near-duplicate (visually the same gradient, a couple of pixels
	// wider) is uploaded as its own image, not skipped.
	if code, res := e.upload(token, nick, "photo2.jpg", jpegFile(t, 102, 60)); code != 200 || res[0]["ok"] != true {
		t.Fatalf("near-duplicate upload: %d %v", code, res)
	}
	imgs, err = e.db.Q.CountImagesInFolder(context.Background(), folderID)
	if err != nil {
		t.Fatal(err)
	}
	if imgs != 2 {
		t.Fatalf("images after near-duplicate = %d, want 2", imgs)
	}

	waitFor(t, "thumbnails and hashes", func() bool {
		var n int
		e.db.QueryRow("SELECT COUNT(*) FROM images WHERE thumbnail_ready=1 AND phash IS NOT NULL").Scan(&n)
		return n == 2
	})
	// Force the debounced scan (normally 30 seconds out) to run now instead
	// of waiting for it in the test.
	if _, err := e.db.Exec("UPDATE jobs SET run_at = '2000-01-01T00:00:00.000Z' WHERE type = 'scan_folder_duplicates'"); err != nil {
		t.Fatal(err)
	}

	var pairID, aID, bID string
	waitFor(t, "duplicate flagged", func() bool {
		rows, err := e.db.Q.ListPendingDuplicates(context.Background(), folderID)
		if err != nil || len(rows) != 1 {
			return false
		}
		pairID, aID, bID = rows[0].ID, rows[0].ImageIDA, rows[0].ImageIDB
		return true
	})

	// The folder page shows the flagged pair to an authenticated viewer.
	page := e.authed("GET", folderPath, nil).Body.String()
	if !strings.Contains(page, "Possible duplicates") || !strings.Contains(page, "2 similar photos") {
		t.Fatalf("folder page missing duplicates panel:\n%s", page)
	}
	// Anonymous uploaders never see it.
	if code, _ := e.upload(token, nick, "x.jpg", jpegFile(t, 10, 10)); code != 200 {
		t.Fatalf("unrelated upload: %d", code)
	}
	if rec := e.do("GET", "/upload/"+token, nil, nil); strings.Contains(rec.Body.String(), "Possible duplicates") {
		t.Fatal("anonymous upload page must not mention duplicates")
	}

	// Tag and rate the image that will be deleted, to verify the merge.
	if rec := e.authed("POST", "/images/"+bID+"/tags", url.Values{"name": {"keeper"}}); rec.Code != 200 {
		t.Fatalf("tag: %d %s", rec.Code, rec.Body)
	}
	if rec := e.authed("POST", "/images/"+bID+"/rating", url.Values{"rating": {"5"}}); rec.Code != 200 {
		t.Fatalf("rate: %d %s", rec.Code, rec.Body)
	}

	// Resolve: keep A, delete B. B's tag and rating land on A.
	rec = e.authed("POST", "/duplicates/"+pairID+"/resolve", url.Values{"keep": {aID}})
	if rec.Code != 200 {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "Possible duplicates") {
		t.Fatal("duplicates panel should be empty after resolving the only pair")
	}
	if rec := e.authed("GET", "/images/"+bID+"/original", nil); rec.Code != 404 {
		t.Fatalf("deleted image still accessible: %d", rec.Code)
	}
	tags, err := e.db.Q.ListImageTags(context.Background(), aID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tg := range tags {
		found = found || tg.Name == "keeper"
	}
	if !found {
		t.Fatalf("tag not merged onto kept image: %v", tags)
	}
	kept, err := e.db.Q.GetImageUnchecked(context.Background(), aID)
	if err != nil {
		t.Fatal(err)
	}
	if !kept.Rating.Valid || kept.Rating.Int64 != 5 {
		t.Fatalf("rating not merged onto kept image: %+v", kept.Rating)
	}
	imgs, err = e.db.Q.CountImagesInFolder(context.Background(), folderID)
	if err != nil {
		t.Fatal(err)
	}
	if imgs != 2 { // kept photo + the unrelated x.jpg
		t.Fatalf("images after resolve = %d, want 2", imgs)
	}
}

// TestDuplicateDismiss checks that dismissing a flagged pair clears it from
// the panel without touching either image.
func TestDuplicateDismiss(t *testing.T) {
	e := setup(t)
	rec := e.authed("POST", "/folders", url.Values{"name": {"Trip"}})
	folderPath := rec.Header().Get("HX-Redirect")
	folderID := strings.TrimPrefix(folderPath, "/folders/")
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	token := regexpMatch(t, rec.Body.String(), `/upload/([A-Za-z0-9_-]+)"`)
	rec = e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	nick := rec.Result().Cookies()[0]

	e.upload(token, nick, "one.jpg", jpegFile(t, 100, 60))
	e.upload(token, nick, "two.jpg", jpegFile(t, 102, 60))
	waitFor(t, "thumbnails and hashes", func() bool {
		var n int
		e.db.QueryRow("SELECT COUNT(*) FROM images WHERE thumbnail_ready=1 AND phash IS NOT NULL").Scan(&n)
		return n == 2
	})
	e.db.Exec("UPDATE jobs SET run_at = '2000-01-01T00:00:00.000Z' WHERE type = 'scan_folder_duplicates'")

	var pairID string
	waitFor(t, "duplicate flagged", func() bool {
		rows, err := e.db.Q.ListPendingDuplicates(context.Background(), folderID)
		if err != nil || len(rows) != 1 {
			return false
		}
		pairID = rows[0].ID
		return true
	})

	rec = e.authed("POST", "/duplicates/"+pairID+"/dismiss", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Possible duplicates") {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	imgs, err := e.db.Q.CountImagesInFolder(context.Background(), folderID)
	if err != nil {
		t.Fatal(err)
	}
	if imgs != 2 {
		t.Fatalf("images after dismiss = %d, want 2 (both kept)", imgs)
	}
	rows, err := e.db.Q.ListPendingDuplicates(context.Background(), folderID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pending duplicates after dismiss = %v, %v", rows, err)
	}

	// Dismissing again (already resolved) 404s.
	if rec := e.authed("POST", "/duplicates/"+pairID+"/dismiss", nil); rec.Code != nethttp.StatusNotFound {
		t.Fatalf("re-dismiss: %d", rec.Code)
	}
}

// TestDuplicateClusters checks that pairs sharing an image are shown as one
// group: with A~B and A~C flagged (but not B~C), all three photos appear in
// the same box, a photo can be taken out of the group, and keeping one
// trashes every other photo of the group.
func TestDuplicateClusters(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxImagesPerFolder = 10 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	rec := e.authed("POST", "/folders", url.Values{"name": {"Party"}})
	folderPath := rec.Header().Get("HX-Redirect")
	folderID := strings.TrimPrefix(folderPath, "/folders/")
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	token := regexpMatch(t, rec.Body.String(), `/upload/([A-Za-z0-9_-]+)"`)
	rec = e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Cy"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	nick := rec.Result().Cookies()[0]

	for i, w := range []int{100, 102, 104, 106} {
		if code, res := e.upload(token, nick, fmt.Sprintf("p%d.jpg", i), jpegFile(t, w, 60)); code != 200 || res[0]["ok"] != true {
			t.Fatalf("upload %d: %d %v", i, code, res)
		}
	}
	waitFor(t, "thumbnails and hashes", func() bool {
		var n int
		e.db.QueryRow("SELECT COUNT(*) FROM images WHERE thumbnail_ready=1 AND phash IS NOT NULL").Scan(&n)
		return n == 4
	})
	e.db.Exec("UPDATE jobs SET run_at = '2000-01-01T00:00:00.000Z' WHERE type = 'scan_folder_duplicates'")
	waitFor(t, "all pairs flagged", func() bool {
		rows, err := e.db.Q.ListPendingDuplicates(ctx, folderID)
		return err == nil && len(rows) == 6
	})

	// Reduce the graph to a chain hub: A~B, A~C, A~D only.
	rows, _ := e.db.Q.ListPendingDuplicates(ctx, folderID)
	a := rows[0].ImageIDA
	for _, r := range rows {
		if r.ImageIDA != a && r.ImageIDB != a {
			if _, err := e.db.Exec("UPDATE image_duplicates SET status='dismissed' WHERE id=?", r.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, _ = e.db.Q.ListPendingDuplicates(ctx, folderID)
	if len(rows) != 3 {
		t.Fatalf("pending pairs = %d, want 3", len(rows))
	}
	others := map[string]bool{}
	for _, r := range rows {
		others[r.ImageIDA], others[r.ImageIDB] = true, true
	}
	delete(others, a)
	var ids []string
	for id := range others {
		ids = append(ids, id)
	}
	pairID := rows[2].ID // any pair addresses the whole group

	// One group of four, shown in a single box.
	page := e.authed("GET", folderPath, nil).Body.String()
	if strings.Count(page, `class="dup-cluster"`) != 1 || !strings.Contains(page, "4 similar photos") {
		t.Fatalf("want one group of 4:\n%s", page)
	}
	for _, id := range append([]string{a}, ids...) {
		if !strings.Contains(page, "/images/"+id+"/thumbnail") {
			t.Fatalf("group missing image %s", id)
		}
	}

	// Taking D out of the group leaves A, B, C together.
	rec = e.authed("POST", "/duplicates/"+pairID+"/exclude", url.Values{"image": {ids[2]}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "3 similar photos") || strings.Contains(rec.Body.String(), ids[2]) {
		t.Fatalf("exclude: %d %s", rec.Code, rec.Body)
	}

	// An image outside the group can't be kept.
	rows, _ = e.db.Q.ListPendingDuplicates(ctx, folderID)
	pairID = rows[0].ID
	if rec := e.authed("POST", "/duplicates/"+pairID+"/resolve", url.Values{"keep": {ids[2]}}); rec.Code != 400 {
		t.Fatalf("keep outside group: %d", rec.Code)
	}

	// Tag B and C differently, rate C; keep A and both get trashed.
	e.authed("POST", "/images/"+ids[0]+"/tags", url.Values{"name": {"from-b"}})
	e.authed("POST", "/images/"+ids[1]+"/tags", url.Values{"name": {"from-c"}})
	e.authed("POST", "/images/"+ids[1]+"/rating", url.Values{"rating": {"4"}})
	rec = e.authed("POST", "/duplicates/"+pairID+"/resolve", url.Values{"keep": {a}})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Possible duplicates") {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	if n, _ := e.db.Q.CountImagesInFolder(ctx, folderID); n != 2 { // A + D
		t.Fatalf("images after resolve = %d, want 2", n)
	}
	tags, _ := e.db.Q.ListImageTags(ctx, a)
	names := map[string]bool{}
	for _, tg := range tags {
		names[tg.Name] = true
	}
	if !names["from-b"] || !names["from-c"] {
		t.Fatalf("tags on kept image = %v, want from-b and from-c", tags)
	}
	kept, _ := e.db.Q.GetImageUnchecked(ctx, a)
	if !kept.Rating.Valid || kept.Rating.Int64 != 4 {
		t.Fatalf("rating on kept image = %+v, want 4", kept.Rating)
	}
}

func regexpMatch(t *testing.T, s, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("pattern %q not found in:\n%s", pattern, s)
	}
	return m[1]
}
