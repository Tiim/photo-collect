package http_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/database/sqlc"
)

func TestClockPage(t *testing.T) {
	e := setup(t)

	// Anonymous visitors see the clock and a sign-in button, no redirect.
	rec := e.do("GET", "/", nil, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `id="clock-qr"`) || !strings.Contains(body, `href="/auth/login"`) {
		t.Fatalf("anonymous /: %d\n%s", rec.Code, body)
	}
	if strings.Contains(body, `href="/folders"`) {
		t.Error("anonymous visitors must not see the folders link")
	}
	for _, p := range []string{"/static/clock.js", "/static/qrcode.js"} {
		if rec := e.do("GET", p, nil, nil); rec.Code != 200 {
			t.Errorf("GET %s: %d", p, rec.Code)
		}
	}

	// Signed-in users get a way back to their folders instead.
	rec = e.do("GET", "/", nil, nil, e.cookie)
	body = rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `href="/folders"`) || strings.Contains(body, `class="button" href="/auth/login"`) {
		t.Fatalf("signed-in /: %d\n%s", rec.Code, body)
	}
}

func TestServerTime(t *testing.T) {
	e := setup(t)
	before := time.Now().UnixMilli()
	rec := e.do("GET", "/time", nil, nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("/time: %d, cache-control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var got struct{ MS int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MS < before || got.MS > time.Now().UnixMilli() {
		t.Errorf("ms = %d, want within [%d, now]", got.MS, before)
	}
}

func TestCaptureTimeInUI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f, err := e.db.Q.CreateFolder(ctx, sqlc.CreateFolderParams{ID: "f1", Name: "party"})
	if err != nil {
		t.Fatal(err)
	}
	add := func(id, device, exif string, offset int64) {
		t.Helper()
		img, err := e.db.Q.InsertImage(ctx, sqlc.InsertImageParams{
			ID: id, FolderID: f.ID, OriginalFilename: id + ".jpg", MimeType: "image/jpeg", SizeBytes: 10, Width: 1, Height: 1,
			Sha256: id + strings.Repeat("a", 64-len(id)), UploaderNickname: "Tim",
			DeviceKey: sqlNullStr(device), ExifTime: sqlNullStr(exif),
		})
		if err != nil {
			t.Fatal(err)
		}
		if offset != 0 {
			if _, err := e.db.Exec("UPDATE images SET time_offset_seconds = ? WHERE id = ?", offset, img.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("fixed", "tim|Apple|iPhone", "2026-09-26T10:00:00", 7500)
	add("open", "tim|Google|Pixel", "2026-09-26T10:00:00", 0)

	page := e.authed("GET", "/images/fixed", nil).Body.String()
	for _, want := range []string{"2026-09-26 12:05:00", "camera clock said 2026-09-26 10:00:00", "&#43;2h 05m 00s"} {
		if !strings.Contains(page, want) {
			t.Errorf("corrected image page lacks %q:\n%s", want, page)
		}
	}
	page = e.authed("GET", "/images/open", nil).Body.String()
	if !strings.Contains(page, "2026-09-26 10:00:00") || !strings.Contains(page, "no clock photo for this camera yet") {
		t.Errorf("uncorrected image page:\n%s", page)
	}

	folder := e.authed("GET", "/folders/f1", nil).Body.String()
	for _, want := range []string{"Apple iPhone", "Google Pixel", "no clock photo yet"} {
		if !strings.Contains(folder, want) {
			t.Errorf("folder page lacks %q:\n%s", want, folder)
		}
	}
}
