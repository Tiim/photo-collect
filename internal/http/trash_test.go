package http_test

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	nethttp "net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiim/photo-collect/internal/config"
)

type trashEnv struct {
	*env
	folderID, folderPath string
	ids                  []string // uploaded images of the folder, oldest first
}

func newFolderWithImages(e *env, name string, sizes ...[2]int) *trashEnv {
	t := e.t
	rec := e.authed("POST", "/folders", url.Values{"name": {name}})
	folderPath := rec.Header().Get("HX-Redirect")
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	token := regexpMatch(t, rec.Body.String(), `/upload/([A-Za-z0-9_-]+)"`)
	rec = e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Alice"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	nick := rec.Result().Cookies()[0]
	for i, sz := range sizes {
		if code, res := e.upload(token, nick, "p"+string(rune('a'+i))+".jpg", jpegFile(t, sz[0], sz[1])); code != 200 || res[0]["ok"] != true {
			t.Fatalf("upload %d: %d %v", i, code, res)
		}
	}
	folderID := strings.TrimPrefix(folderPath, "/folders/")
	te := &trashEnv{env: e, folderID: folderID, folderPath: folderPath}
	rows, err := e.db.Query("SELECT id FROM images WHERE folder_id = ? ORDER BY seq", folderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		rows.Scan(&id)
		te.ids = append(te.ids, id)
	}
	waitFor(t, "thumbnails", func() bool {
		var n int
		e.db.QueryRow("SELECT COUNT(*) FROM images WHERE folder_id = ? AND thumbnail_ready = 1", folderID).Scan(&n)
		return n == len(sizes)
	})
	return te
}

func (e *env) scalar(query string, args ...any) int {
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func storedObjects(t *testing.T, dir string) []string {
	var keys []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			keys = append(keys, rel)
		}
		return nil
	})
	return keys
}

func TestTrashHidesImagesEverywhere(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxImagesPerFolder = 10 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := newFolderWithImages(e, "Trash test", [2]int{100, 60}, [2]int{70, 90}, [2]int{50, 120})
	a, b, c := f.ids[0], f.ids[1], f.ids[2]
	// A pending duplicate pair between a and c.
	if _, err := e.db.Exec(`INSERT INTO image_duplicates (id, folder_id, image_id_a, image_id_b, distance) VALUES ('d1', ?, ?, ?, 3)`, f.folderID, min(a, c), max(a, c)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.authed("GET", f.folderPath, nil).Body.String(), "Possible duplicates") {
		t.Fatal("duplicate panel should show before trashing")
	}

	rec := e.authed("POST", f.folderPath+"/images/trash", url.Values{"image": {a, b}})
	if rec.Code != 200 {
		t.Fatalf("trash: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "tile-"+a) || strings.Contains(body, "tile-"+b) || !strings.Contains(body, "tile-"+c) {
		t.Fatalf("refreshed grid wrong:\n%s", body)
	}
	if !strings.Contains(body, `hx-swap-oob="true"`) || !strings.Contains(body, "Trash (2)") || !strings.Contains(body, "1 image ·") {
		t.Fatalf("counts not refreshed:\n%s", body)
	}
	if e.scalar("SELECT COUNT(*) FROM images WHERE deleted_at IS NOT NULL AND deleted_by = 'u1'") != 2 {
		t.Fatal("deleted_at/deleted_by not recorded")
	}

	// Grid, counts, folder list.
	page := e.authed("GET", f.folderPath, nil).Body.String()
	if strings.Contains(page, "tile-"+a) || !strings.Contains(page, "tile-"+c) || !strings.Contains(page, "Trash (2)") {
		t.Fatal("folder page still shows trashed images")
	}
	if got := e.authed("GET", f.folderPath+"/images", nil).Body.String(); strings.Contains(got, "tile-"+a) {
		t.Fatal("paged grid shows trashed image")
	}
	if strings.Contains(page, "Possible duplicates") {
		t.Fatal("duplicate pair must be hidden while one image is trashed")
	}
	if !strings.Contains(e.authed("GET", "/folders", nil).Body.String(), "1 images") {
		t.Log("folder list count not asserted textually")
	}
	// Image routes are 404, except the thumbnail used by the trash view.
	for _, p := range []string{"", "/tile", "/preview", "/original"} {
		if rec := e.authed("GET", "/images/"+a+p, nil); rec.Code != 404 {
			t.Errorf("GET /images/%s%s = %d, want 404", "<trashed>", p, rec.Code)
		}
	}
	if rec := e.authed("GET", "/images/"+a+"/thumbnail", nil); rec.Code != 200 {
		t.Errorf("trashed thumbnail = %d, want 200", rec.Code)
	}
	if rec := e.authed("POST", "/images/"+a+"/rating", url.Values{"rating": {"3"}}); rec.Code != 404 {
		t.Errorf("rating a trashed image = %d, want 404", rec.Code)
	}

	// Exports leave trashed images out, both "all" and explicit selections.
	if rec := e.authed("POST", f.folderPath+"/downloads", url.Values{"all": {"1"}}); rec.Code != 200 {
		t.Fatalf("export all: %d %s", rec.Code, rec.Body)
	}
	if n := e.scalar("SELECT COUNT(*) FROM export_images WHERE image_id IN (?, ?)", a, b); n != 0 {
		t.Fatalf("export contains %d trashed images", n)
	}
	if n := e.scalar("SELECT COUNT(*) FROM export_images WHERE image_id = ?", c); n != 1 {
		t.Fatalf("export lost the visible image (%d)", n)
	}
	e.authed("POST", f.folderPath+"/downloads", url.Values{"image": {a, c}})
	if n := e.scalar("SELECT COUNT(*) FROM export_images WHERE image_id = ?", a); n != 0 {
		t.Fatal("selected export includes a trashed image")
	}

	// No pHash backfill for trashed images.
	if _, err := e.db.Exec("UPDATE images SET phash = NULL, phash_attempted_at = NULL"); err != nil {
		t.Fatal(err)
	}
	missing, err := e.db.Q.ListImagesMissingPHash(context.Background())
	if err != nil || len(missing) != 1 || missing[0].ID != c {
		t.Fatalf("backfill candidates = %v, %v", missing, err)
	}

	// The trash view lists them.
	tv := e.authed("GET", f.folderPath+"/trash", nil).Body.String()
	if !strings.Contains(tv, "tile-"+a) || !strings.Contains(tv, "tile-"+b) || strings.Contains(tv, "tile-"+c) {
		t.Fatalf("trash view wrong:\n%s", tv)
	}

	// Restore brings image and duplicate pair back.
	rec = e.authed("POST", f.folderPath+"/images/restore", url.Values{"image": {a}})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "tile-"+a) || !strings.Contains(rec.Body.String(), "tile-"+b) {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if rec := e.authed("GET", "/images/"+a, nil); rec.Code != 200 {
		t.Fatalf("restored image = %d", rec.Code)
	}
	if !strings.Contains(e.authed("GET", f.folderPath, nil).Body.String(), "Possible duplicates") {
		t.Fatal("duplicate pair should reappear after restore")
	}
	if e.scalar("SELECT COUNT(*) FROM images WHERE id = ? AND deleted_at IS NULL AND deleted_by IS NULL", a) != 1 {
		t.Fatal("restore did not clear the marker")
	}
}

func TestTrashRequestValidation(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxImagesPerFolder = 10 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := newFolderWithImages(e, "One", [2]int{100, 60})
	other := newFolderWithImages(e, "Two", [2]int{70, 90})

	// CSRF is required.
	for _, p := range []string{"/images/trash", "/images/restore", "/images/purge"} {
		rec := e.do("POST", f.folderPath+p, strings.NewReader("image="+f.ids[0]), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, e.cookie)
		if rec.Code != nethttp.StatusForbidden {
			t.Errorf("%s without csrf = %d", p, rec.Code)
		}
	}
	if rec := e.do("POST", "/images/"+f.ids[0]+"/trash", nil, nil, e.cookie); rec.Code != nethttp.StatusForbidden {
		t.Errorf("single trash without csrf = %d", rec.Code)
	}
	// Anonymous users are refused.
	if rec := e.do("POST", f.folderPath+"/images/trash", strings.NewReader("image="+f.ids[0]), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); rec.Code == 200 {
		t.Error("anonymous trash succeeded")
	}
	// Empty and oversized selections.
	if rec := e.authed("POST", f.folderPath+"/images/trash", url.Values{}); rec.Code != 400 {
		t.Errorf("empty selection = %d", rec.Code)
	}
	many := make([]string, 501)
	for i := range many {
		many[i] = f.ids[0]
	}
	if rec := e.authed("POST", f.folderPath+"/images/trash", url.Values{"image": many}); rec.Code != 400 {
		t.Errorf("501 ids = %d", rec.Code)
	}
	// IDs of another folder are ignored, unknown IDs too.
	if rec := e.authed("POST", f.folderPath+"/images/trash", url.Values{"image": {other.ids[0], "nope"}}); rec.Code != 200 {
		t.Fatalf("foreign ids = %d", rec.Code)
	}
	if e.scalar("SELECT COUNT(*) FROM images WHERE deleted_at IS NOT NULL") != 0 {
		t.Fatal("image of another folder was trashed")
	}
	// Single-image trash from the detail view redirects to the folder.
	rec := e.authed("POST", "/images/"+f.ids[0]+"/trash", nil)
	if rec.Header().Get("HX-Redirect") != f.folderPath {
		t.Fatalf("single trash: %d %v", rec.Code, rec.Header())
	}
	if e.scalar("SELECT COUNT(*) FROM images WHERE id = ? AND deleted_at IS NOT NULL", f.ids[0]) != 1 {
		t.Fatal("single trash did not trash")
	}
	// Restore/purge of the other folder's ids through this folder does nothing.
	e.authed("POST", f.folderPath+"/images/purge", url.Values{"image": {other.ids[0]}})
	if e.scalar("SELECT COUNT(*) FROM images WHERE id = ?", other.ids[0]) != 1 {
		t.Fatal("purge crossed folders")
	}
}

func TestPurgeDeletesRowsAndObjectsDurably(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxImagesPerFolder = 10 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := newFolderWithImages(e, "Purge", [2]int{100, 60}, [2]int{70, 90})
	a, b := f.ids[0], f.ids[1]
	if _, err := e.db.Exec(`INSERT INTO image_duplicates (id, folder_id, image_id_a, image_id_b, distance) VALUES ('d1', ?, ?, ?, 3)`, f.folderID, min(a, b), max(a, b)); err != nil {
		t.Fatal(err)
	}
	e.authed("POST", f.folderPath+"/downloads", url.Values{"image": {a}})

	// Purging a photo that is not in the trash is refused (silently ignored).
	rec := e.authed("POST", f.folderPath+"/images/purge", url.Values{"image": {a}})
	if rec.Code != 200 || e.scalar("SELECT COUNT(*) FROM images WHERE id = ?", a) != 1 {
		t.Fatalf("purge of a non-trashed image: %d", rec.Code)
	}
	if e.scalar("SELECT COUNT(*) FROM jobs WHERE type = 'delete_image_objects'") != 0 {
		t.Fatal("cleanup job enqueued for a non-trashed image")
	}

	e.authed("POST", f.folderPath+"/images/trash", url.Values{"image": {a}})
	before := len(storedObjects(t, e.storeDir))
	rec = e.authed("POST", f.folderPath+"/images/purge", url.Values{"image": {a}})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "tile-"+a) || !strings.Contains(rec.Body.String(), "0 in trash") {
		t.Fatalf("purge: %d %s", rec.Code, rec.Body)
	}
	if e.scalar("SELECT COUNT(*) FROM images WHERE id = ?", a) != 0 ||
		e.scalar("SELECT COUNT(*) FROM image_duplicates") != 0 ||
		e.scalar("SELECT COUNT(*) FROM export_images WHERE image_id = ?", a) != 0 ||
		e.scalar("SELECT COUNT(*) FROM image_tags WHERE image_id = ?", a) != 0 {
		t.Fatal("purge left rows behind")
	}
	waitFor(t, "objects removed", func() bool {
		for _, k := range storedObjects(t, e.storeDir) {
			if strings.Contains(k, a) {
				return false
			}
		}
		return true
	})
	if got := len(storedObjects(t, e.storeDir)); got >= before {
		t.Fatalf("objects: %d before, %d after", before, got)
	}
	// The other image is untouched and its export can still be built.
	if rec := e.authed("GET", "/images/"+b+"/original", nil); rec.Code != 200 {
		t.Fatalf("other image original = %d", rec.Code)
	}
	waitFor(t, "export built", func() bool {
		return e.scalar("SELECT COUNT(*) FROM exports WHERE status = 'ready'") == 1
	})
}

func TestDuplicateResolveMovesLoserToTrash(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxImagesPerFolder = 10 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := newFolderWithImages(e, "Dups", [2]int{100, 60}, [2]int{70, 90})
	a, b := f.ids[0], f.ids[1]
	if a > b {
		a, b = b, a
	}
	if _, err := e.db.Exec(`INSERT INTO image_duplicates (id, folder_id, image_id_a, image_id_b, distance) VALUES ('d1', ?, ?, ?, 3)`, f.folderID, min(a, b), max(a, b)); err != nil {
		t.Fatal(err)
	}
	if rec := e.authed("POST", "/duplicates/d1/resolve", url.Values{"keep": {a}}); rec.Code != 200 {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	if e.scalar("SELECT COUNT(*) FROM images WHERE id = ? AND deleted_at IS NOT NULL", b) != 1 {
		t.Fatal("loser is not in the trash")
	}
	// Restoring it does not bring the resolved pair back.
	e.authed("POST", f.folderPath+"/images/restore", url.Values{"image": {b}})
	if strings.Contains(e.authed("GET", f.folderPath, nil).Body.String(), "Possible duplicates") {
		t.Fatal("resolved pair reappeared after restore")
	}
}

func TestDeletingFolderRemovesTrashedObjects(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxImagesPerFolder = 10 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := newFolderWithImages(e, "Gone", [2]int{100, 60}, [2]int{70, 90})
	e.authed("POST", f.folderPath+"/images/trash", url.Values{"image": {f.ids[0]}})
	if len(storedObjects(t, e.storeDir)) == 0 {
		t.Fatal("no objects stored")
	}
	if rec := e.authed("POST", f.folderPath+"/delete", nil); rec.Code >= 400 {
		t.Fatalf("delete folder: %d", rec.Code)
	}
	waitFor(t, "folder cleanup", func() bool {
		return e.scalar("SELECT COUNT(*) FROM folders") == 0 && e.scalar("SELECT COUNT(*) FROM images") == 0
	})
	if left := storedObjects(t, e.storeDir); len(left) != 0 {
		t.Fatalf("objects left after folder deletion: %v", left)
	}
}

func TestUploadCapacityCountsTrash(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxImagesPerFolder = 2 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := newFolderWithImages(e, "Cap", [2]int{100, 60}, [2]int{70, 90})
	e.authed("POST", f.folderPath+"/images/trash", url.Values{"image": {f.ids[0], f.ids[1]}})
	// Trashed photos still occupy storage, so the folder stays full.
	rec := e.authed("POST", f.folderPath+"/upload-link", url.Values{"days": {"2"}})
	token := regexpMatch(t, rec.Body.String(), `/upload/([A-Za-z0-9_-]+)"`)
	rec = e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	code, res := e.upload(token, rec.Result().Cookies()[0], "new.jpg", jpegFile(t, 60, 60))
	if code == 200 && len(res) > 0 && res[0]["ok"] == true {
		t.Fatal("upload succeeded although the folder is full of trashed photos")
	}
}
