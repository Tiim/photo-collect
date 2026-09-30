package http_test

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/filter"
)

type seed struct {
	id, uploader, exif string
	rating             int
	offset             int // seconds, applied when non-zero
	tags               []string
}

// seedFolder creates folder "g1" with the given images, uploaded in order.
func seedFolder(t *testing.T, e *env, seeds []seed) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.db.Q.CreateFolder(ctx, sqlc.CreateFolderParams{ID: "g1", Name: "Gallery"}); err != nil {
		t.Fatal(err)
	}
	for i, s := range seeds {
		_, err := e.db.Q.InsertImage(ctx, sqlc.InsertImageParams{
			ID: s.id, FolderID: "g1", OriginalFilename: s.id + ".jpg", MimeType: "image/jpeg", SizeBytes: 10, Width: 1, Height: 1,
			Sha256: fmt.Sprintf("%064d", i), UploaderNickname: s.uploader, ExifTime: sqlNullStr(s.exif),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s.exif, "20") {
			e.db.Exec("UPDATE images SET exif_time = NULL WHERE id = ?", s.id)
		}
		if s.rating > 0 {
			e.db.Q.SetImageRating(ctx, sqlc.SetImageRatingParams{Rating: sqlNullInt(int64(s.rating)), ID: s.id})
		}
		if s.offset != 0 {
			e.db.Exec("UPDATE images SET time_offset_seconds = ? WHERE id = ?", s.offset, s.id)
		}
		for _, tg := range s.tags {
			tag, err := e.db.Q.UpsertTag(ctx, tg)
			if err != nil {
				t.Fatal(err)
			}
			e.db.Q.AddImageTag(ctx, sqlc.AddImageTagParams{ImageID: s.id, TagID: tag.ID})
		}
	}
}

var gallerySeeds = []seed{
	{id: "a", uploader: "Anna", rating: 5, exif: "2026-06-01T10:00:00", tags: []string{"forrest", "summer"}},
	{id: "b", uploader: "Anna", rating: 2, exif: "2026-06-01T23:30:00", tags: []string{"forrest"}},
	{id: "c", uploader: "Ben", rating: 3, exif: "2026-06-02T09:00:00", tags: []string{"summer"}},
	{id: "d", uploader: "Ben", rating: 3, exif: "2026-06-02T09:00:00", tags: []string{"forrest", "summer", "lake"}},
	// The camera clock was 1 h behind: corrected time is 2026-06-02 00:10, not 06-01 23:10.
	{id: "e", uploader: "Cleo", exif: "2026-06-01T23:10:00", offset: 3600},
	{id: "f", uploader: "Cleo", rating: 4}, // no capture time
}

func matching(t *testing.T, e *env, query string) []string {
	t.Helper()
	v, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	f, err := filter.Parse(v)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := e.db.Q.ListFilteredRefs(context.Background(), f.RefsParams("g1", 100))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	n, err := e.db.Q.CountImagesFiltered(context.Background(), f.CountParams("g1"))
	if err != nil || int(n) != len(ids) {
		t.Fatalf("%q: count = %d, %v; list has %d", query, n, err, len(ids))
	}
	rows, err := e.db.Q.ListImagesFiltered(context.Background(), f.ListParams("g1", nil, 100))
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, r := range rows {
		listed = append(listed, r.ID)
	}
	sort.Strings(listed)
	got := append([]string(nil), ids...)
	sort.Strings(got)
	if !reflect.DeepEqual(listed, got) {
		t.Fatalf("%q: list query %v disagrees with refs query %v", query, listed, got)
	}
	return ids
}

func TestFilterQuerySemantics(t *testing.T) {
	e := setup(t)
	seedFolder(t, e, gallerySeeds)
	e.db.Exec("UPDATE images SET gps_lat = 1.5, gps_lon = 2.5 WHERE id IN ('b', 'e')")

	for _, c := range []struct {
		name, query string
		want        []string
	}{
		{"no filter", "", []string{"a", "b", "c", "d", "e", "f"}},
		{"tag", "tag=forrest", []string{"a", "b", "d"}},
		{"tag AND", "tag=forrest&tag=summer", []string{"a", "d"}},
		{"tag AND three", "tag=forrest&tag=summer&tag=lake", []string{"d"}},
		{"tag OR", "tag=lake&tag=summer&tag_mode=any", []string{"a", "c", "d"}},
		{"tag unknown", "tag=nothing", nil},
		{"tag case", "tag=FORREST", []string{"a", "b", "d"}},
		{"rating min", "rating_min=3", []string{"a", "c", "d", "f"}},
		{"rating max", "rating_max=3", []string{"b", "c", "d"}},
		{"rating range", "rating_min=3&rating_max=4", []string{"c", "d", "f"}},
		{"forrest and rating >= 3 (the example)", "tag=forrest&rating_min=3", []string{"a", "d"}},
		{"uploader", "uploader=Ben", []string{"c", "d"}},
		{"uploader unknown", "uploader=Zed", nil},
		{"from", "from=2026-06-02", []string{"c", "d", "e"}},
		{"to", "to=2026-06-01", []string{"a", "b"}},
		{"day range", "from=2026-06-02&to=2026-06-02", []string{"c", "d", "e"}},
		{"has gps", "has_gps=1", []string{"b", "e"}},
		{"combined", "tag=forrest&uploader=Anna&from=2026-06-01&to=2026-06-01&rating_max=2", []string{"b"}},
	} {
		got := matching(t, e, c.query)
		sort.Strings(got)
		if !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("%s (%q): got %v, want %v", c.name, c.query, got, c.want)
		}
	}
}

func TestFilterExcludesTrashed(t *testing.T) {
	e := setup(t)
	seedFolder(t, e, gallerySeeds)
	e.db.Exec("UPDATE images SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = 'a'")
	got := matching(t, e, "tag=forrest")
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"b", "d"}) {
		t.Fatalf("got %v", got)
	}
}

var (
	tileRE     = regexp.MustCompile(`<div class="tile[^"]*" id="tile-([^"]+)"`)
	sentinelRE = regexp.MustCompile(`hx-get="(/folders/g1/images\?[^"]+)"`)
)

// pageIDs walks all pages of a query the way the browser does (first page from
// the gallery fragment, then the paging sentinel) and returns the image IDs in
// the order shown.
func pageIDs(t *testing.T, e *env, query string) []string {
	t.Helper()
	var ids []string
	path := "/folders/g1/gallery?" + query
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("paging does not end")
		}
		rec := e.authed("GET", path, nil)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		for _, m := range tileRE.FindAllStringSubmatch(rec.Body.String(), -1) {
			ids = append(ids, m[1])
		}
		next := sentinelRE.FindStringSubmatch(rec.Body.String())
		if next == nil {
			return ids
		}
		path = html.UnescapeString(next[1])
	}
}

// Paging is a keyset on (sort key, seq): with many ties (same rating, same
// capture second) no photo may be skipped or repeated, in any sort order.
func TestFilterPagingStableWithTies(t *testing.T) {
	e := setup(t)
	var seeds []seed
	for i := 0; i < 150; i++ {
		seeds = append(seeds, seed{
			id: fmt.Sprintf("i%03d", i), uploader: "Anna", rating: i % 3, // many ties
			exif: fmt.Sprintf("2026-06-01T10:00:%02d", i%5), tags: []string{"t"},
		})
	}
	seedFolder(t, e, seeds)

	for _, q := range []string{
		"", "dir=asc", "sort=rating", "sort=rating&dir=asc", "sort=captured", "sort=captured&dir=asc",
		"sort=rating&tag=t&rating_min=1",
	} {
		v, _ := url.ParseQuery(q)
		f, err := filter.Parse(v)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := e.db.Q.ListImagesFiltered(context.Background(), f.ListParams("g1", nil, 1000))
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, r := range rows {
			want = append(want, r.ID)
		}
		got := pageIDs(t, e, q)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: paged order differs from the single-page order (%d vs %d images, first pages %v vs %v)",
				q, len(got), len(want), got[:min(5, len(got))], want[:min(5, len(want))])
		}
		if q == "" && len(got) != 150 {
			t.Errorf("unfiltered: %d images over the pages", len(got))
		}
	}

	// Sort semantics.
	byRating := pageIDs(t, e, "sort=rating&dir=asc")
	if byRating[0] != "i000" || len(byRating) != 150 { // rating NULL (unrated) sorts first
		t.Errorf("rating asc starts with %v", byRating[:3])
	}
	newest := pageIDs(t, e, "")
	if newest[0] != "i149" || newest[149] != "i000" {
		t.Errorf("default order: %v ... %v", newest[0], newest[149])
	}
}

// Sorting by capture time uses the clock-corrected time.
func TestFilterSortsByCorrectedTime(t *testing.T) {
	e := setup(t)
	seedFolder(t, e, gallerySeeds)
	got := pageIDs(t, e, "sort=captured&dir=asc")
	// f has no capture time and sorts first; e (23:10 + 1 h) comes after b (23:30 the day before).
	want := []string{"f", "a", "b", "e", "c", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFolderPageFilter(t *testing.T) {
	e := setup(t)
	seedFolder(t, e, gallerySeeds)

	rec := e.authed("GET", "/folders/g1/gallery?tag=forrest&rating_min=3", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if got := rec.Header().Get("HX-Push-Url"); got != "/folders/g1?rating_min=3&tag=forrest" {
		t.Errorf("HX-Push-Url = %q", got)
	}
	if !strings.Contains(body, "2 of 6 photos match") {
		t.Errorf("match count missing:\n%s", body)
	}
	if ids := tileRE.FindAllStringSubmatch(body, -1); len(ids) != 2 {
		t.Errorf("tiles = %v", ids)
	}
	if !strings.Contains(body, "Download all matching") || !strings.Contains(body, "Move all matching to trash") {
		t.Errorf("matching actions missing")
	}

	// The full page carries the filter bar with the applied values and the same grid.
	rec = e.do("GET", "/folders/g1?tag=forrest&rating_min=3&uploader=Ben", nil, nil, e.cookie)
	body = rec.Body.String()
	for _, want := range []string{`<!doctype html>`, `id="filter-form"`, `value="forrest"`, `<option value="3" selected>`, `<option value="Ben" selected>`, `id="gallery"`} {
		if !strings.Contains(body, want) {
			t.Errorf("folder page lacks %q", want)
		}
	}

	// No filter: no matching actions, plain count.
	body = e.authed("GET", "/folders/g1/gallery", nil).Body.String()
	if strings.Contains(body, "matching") || !strings.Contains(body, "6 photos") {
		t.Errorf("unfiltered gallery:\n%s", body)
	}
	if got := e.authed("GET", "/folders/g1/gallery?tag=nothing", nil).Body.String(); !strings.Contains(got, "No photos match this filter") {
		t.Errorf("empty result message missing")
	}

	for _, bad := range []string{"rating_min=9", "from=x", "sort=bogus"} {
		for _, p := range []string{"/folders/g1/gallery?", "/folders/g1?", "/folders/g1/images?"} {
			if rec := e.authed("GET", p+bad, nil); rec.Code != 400 {
				t.Errorf("GET %s%s = %d, want 400", p, bad, rec.Code)
			}
		}
	}
	if rec := e.authed("GET", "/folders/g1/images?after_seq=zz", nil); rec.Code != 400 {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	if rec := e.authed("GET", "/folders/nope/gallery", nil); rec.Code != 404 {
		t.Errorf("unknown folder = %d", rec.Code)
	}
	// Trashed photos never show.
	e.db.Exec("UPDATE images SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = 'a'")
	if body := e.authed("GET", "/folders/g1/gallery?tag=forrest&rating_min=3", nil).Body.String(); strings.Contains(body, `tile-a"`) || !strings.Contains(body, "1 of 5 photos match") {
		t.Errorf("trashed photo listed or counted:\n%s", body)
	}
}

func TestTagSuggestForFilterKeepsEarlierTags(t *testing.T) {
	e := setup(t)
	seedFolder(t, e, gallerySeeds)
	body := e.authed("GET", "/tags/suggest?"+url.Values{"tag": {"forrest, su"}}.Encode(), nil).Body.String()
	if !strings.Contains(body, `<option value="forrest, summer">`) || strings.Contains(body, `"lake"`) {
		t.Errorf("suggestions = %s", body)
	}
}

func TestTrashAllMatching(t *testing.T) {
	e := setup(t)
	seedFolder(t, e, gallerySeeds)
	post := func(target string) int { return e.authed("POST", target, url.Values{}).Code }

	if got := post("/folders/g1/images/trash?matching=1&expect=6"); got != 400 {
		t.Errorf("matching without filter = %d, want 400 (never trash a whole folder this way)", got)
	}
	if got := post("/folders/g1/images/trash?tag=forrest&matching=1&expect=2"); got != 409 {
		t.Errorf("stale count = %d, want 409", got)
	}
	if got := post("/folders/g1/images/trash?tag=nothing&matching=1&expect=0"); got != 400 {
		t.Errorf("empty match = %d, want 400", got)
	}
	if n := e.scalar("SELECT COUNT(*) FROM images WHERE deleted_at IS NOT NULL"); n != 0 {
		t.Fatalf("%d images trashed by refused requests", n)
	}
	rec := e.authed("POST", "/folders/g1/images/trash?tag=forrest&matching=1&expect=3", url.Values{})
	if rec.Code != 200 {
		t.Fatalf("trash matching = %d %s", rec.Code, rec.Body)
	}
	if n := e.scalar("SELECT COUNT(*) FROM images WHERE deleted_at IS NOT NULL AND id IN ('a','b','d') AND deleted_by = 'u1'"); n != 3 {
		t.Errorf("%d of the 3 matching images trashed", n)
	}
	if n := e.scalar("SELECT COUNT(*) FROM images WHERE deleted_at IS NULL"); n != 3 {
		t.Errorf("%d images left", n)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "0 of 3 photos match") || !strings.Contains(body, "Trash (3)") {
		t.Errorf("response does not show the new counts:\n%s", body)
	}
	// No CSRF token, no trashing.
	if rec := e.do("POST", "/folders/g1/images/trash?tag=summer&matching=1&expect=2", nil, nil, e.cookie); rec.Code != 403 {
		t.Errorf("without CSRF = %d", rec.Code)
	}
}

func TestExportAllMatching(t *testing.T) {
	e := setup(t)
	seedFolder(t, e, gallerySeeds)
	if got := e.authed("POST", "/folders/g1/downloads?rating_min=9&matching=1", url.Values{}).Code; got != 400 {
		t.Errorf("bad filter = %d", got)
	}
	if got := e.authed("POST", "/folders/g1/downloads?tag=nothing&matching=1", url.Values{}).Code; got != 400 {
		t.Errorf("empty match = %d", got)
	}
	if rec := e.authed("POST", "/folders/g1/downloads?uploader=Ben&matching=1", url.Values{}); rec.Code != 200 {
		t.Fatalf("export = %d %s", rec.Code, rec.Body)
	}
	if n := e.scalar("SELECT COUNT(*) FROM export_images WHERE image_id IN ('c', 'd')"); n != 2 {
		t.Errorf("export holds %d of the 2 matching images", n)
	}
	if n := e.scalar("SELECT COUNT(*) FROM export_images"); n != 2 {
		t.Errorf("export holds %d images, want only the matches", n)
	}
}

// The filter must not turn into a full scan of every image on big folders.
func TestFilterQueryPlanUsesIndexes(t *testing.T) {
	e := setup(t)
	if _, err := e.db.Q.CreateFolder(context.Background(), sqlc.CreateFolderParams{ID: "big", Name: "Big"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 5000)
		INSERT INTO images (id, folder_id, original_filename, mime_type, size_bytes, width, height, sha256, uploader_nickname, rating)
		SELECT 'b' || i, 'big', 'x.jpg', 'image/jpeg', 1, 1, 1, printf('%064d', i), 'u' || (i % 7), CASE WHEN i % 6 = 0 THEN NULL ELSE i % 5 + 1 END FROM n`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct{ name, sql string }{
		{"count with rating", "SELECT COUNT(*) FROM images i WHERE i.folder_id = 'big' AND i.deleted_at IS NULL AND i.rating >= 3"},
		{"tag lookup", "SELECT image_id FROM image_tags WHERE tag_id = 1"},
	} {
		rows, err := e.db.Query("EXPLAIN QUERY PLAN " + q.sql)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			rows.Scan(&id, &parent, &unused, &detail)
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		if strings.Contains(joined, "SCAN images") || strings.Contains(joined, "SCAN image_tags") {
			t.Errorf("%s: full table scan: %s", q.name, joined)
		}
	}
	// And the real filtered query answers quickly on 5000 images.
	f, _ := filter.Parse(url.Values{"rating_min": {"3"}, "uploader": {"u1"}, "sort": {"rating"}})
	rows, err := e.db.Q.ListImagesFiltered(context.Background(), f.ListParams("big", nil, 61))
	if err != nil || len(rows) != 61 {
		t.Fatalf("filtered list = %d rows, %v", len(rows), err)
	}
}
