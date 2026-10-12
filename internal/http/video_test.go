package http_test

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"net/url"

	"github.com/tiim/photo-collect/internal/images/imagetest"
)

func TestVideoUploadGalleryAndExport(t *testing.T) {
	e := setup(t) // images up to 1 MiB, videos up to 2 MiB
	token, folderPath := e.newLink("Wedding")
	nick := e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), form).Result().Cookies()[0]

	if page := e.do("GET", "/upload/"+token, nil, nil, nick).Body.String(); !strings.Contains(page, `accept="image/*,video/*,.heic,.heif"`) || !strings.Contains(page, "up to 2 MB") {
		t.Fatalf("upload page does not offer videos:\n%s", page)
	}

	video := imagetest.Video("qt  ", []string{"qt  "}, 1500<<10)
	if code, res := e.upload(token, nick, "IMG_0001.MOV", video); code != 200 || res[0]["ok"] != true {
		t.Fatalf("upload: %d %v", code, res)
	}
	if code, res := e.upload(token, nick, "again.mov", video); code != 200 || res[0]["ok"] != true {
		t.Fatalf("re-upload: %d %v", code, res)
	}
	if n := e.scalar("SELECT COUNT(*) FROM images"); n != 1 {
		t.Fatalf("%d rows after re-upload, want 1", n)
	}
	var id string
	e.db.QueryRow("SELECT id FROM images").Scan(&id)

	// Derivation and the clock scan finish at once, without retries.
	const imageJobs = "type IN ('derive_image', 'analyze_image')"
	waitFor(t, "image jobs", func() bool {
		return e.scalar("SELECT COUNT(*) FROM jobs WHERE "+imageJobs+" AND status = 'done'") == 2
	})
	if n := e.scalar("SELECT COUNT(*) FROM jobs WHERE " + imageJobs + " AND attempts != 1"); n != 0 {
		t.Errorf("%d jobs were retried", n)
	}
	if n := e.scalar("SELECT COUNT(*) FROM images WHERE phash_attempted_at IS NOT NULL AND qr_scanned = 1 AND thumbnail_ready = 0"); n != 1 {
		t.Errorf("video not marked as processed")
	}

	// Over the video limit, and not a supported file at all.
	if code, res := e.upload(token, nick, "long.mov", imagetest.Video("qt  ", nil, 3<<20)); code != 400 || !strings.Contains(res[0]["error"].(string), "max 2 MB") {
		t.Errorf("too large: %d %v", code, res)
	}
	if code, res := e.upload(token, nick, "anim.heic", imagetest.Video("msf1", []string{"msf1"}, 10)); code != 200 || res[0]["ok"] == true {
		t.Errorf("heif sequence: %d %v", code, res)
	}

	// The gallery shows a video tile that does not poll for a thumbnail.
	page := e.authed("GET", folderPath, nil).Body.String()
	tile := page[strings.Index(page, `id="tile-`+id+`"`):]
	tile = tile[:strings.Index(tile, "</div>")]
	if strings.Contains(tile, "hx-get=\"/images/"+id+"/tile\"") {
		t.Errorf("video tile polls for a thumbnail:\n%s", tile)
	}
	if !strings.Contains(page, `placeholder video`) || !strings.Contains(page, "IMG_0001.MOV") {
		t.Errorf("no video tile:\n%s", page)
	}
	detail := e.authed("GET", "/images/"+id, nil).Body.String()
	if !strings.Contains(detail, "placeholder video") || !strings.Contains(detail, "/images/"+id+"/original?download=1") || strings.Contains(detail, "/preview") {
		t.Errorf("detail:\n%s", detail)
	}

	// The original comes back byte-identical, alone and in the export.
	want := sha256.Sum256(video)
	if rec := e.authed("GET", "/images/"+id+"/original?download=1", nil); rec.Code != 200 || sha256.Sum256(rec.Body.Bytes()) != want ||
		rec.Header().Get("Content-Type") != "video/quicktime" {
		t.Fatalf("original: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	entries := e.readZip("GET", folderPath+"/export.zip?all=1", nil)
	if len(entries) != 1 || sha256.Sum256(entries["Wedding/IMG_0001.MOV"]) != want {
		t.Fatalf("export holds %d entries, video identical: %v", len(entries), bytes.Equal(entries["Wedding/IMG_0001.MOV"], video))
	}
	if l := e.exportList(folderPath+"/export.json?all=1", nil); len(l.Files) != 1 || l.Files[0].XMPName != "" || l.Files[0].XMP != "" {
		t.Fatalf("export list: %+v", l.Files)
	}

	// Trash, restore, trash again and purge.
	sel := url.Values{"image": {id}}
	for _, step := range []string{"trash", "restore", "trash", "purge"} {
		if rec := e.authed("POST", folderPath+"/images/"+step, sel); rec.Code != 200 {
			t.Fatalf("%s: %d %s", step, rec.Code, rec.Body)
		}
		if step == "trash" && !strings.Contains(e.authed("GET", folderPath+"/trash", nil).Body.String(), "placeholder video") {
			t.Errorf("trash does not show the video tile")
		}
	}
	if n := e.scalar("SELECT COUNT(*) FROM images"); n != 0 {
		t.Fatalf("%d rows after purge", n)
	}
}
