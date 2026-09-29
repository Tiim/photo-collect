package http_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/config"
	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/downloads"
	apphttp "github.com/tiim/photo-collect/internal/http"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/oidc"
	"github.com/tiim/photo-collect/internal/sessions"
	"github.com/tiim/photo-collect/internal/storage/filesystem"
	"github.com/tiim/photo-collect/internal/uploads"
)

type env struct {
	t        *testing.T
	h        nethttp.Handler
	db       *database.DB
	storeDir string
	cookie   *nethttp.Cookie
	csrf     string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := database.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := filesystem.New(filepath.Join(dir, "photos"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BaseURL: "http://example.test", SessionSecret: strings.Repeat("s", 32), SessionTTL: time.Hour,
		UploadMaxFileSize: 1 << 20, UploadMaxFilesPerRequest: 5, UploadMaxImagesPerFolder: 3,
		UploadLinkDuration: 24 * time.Hour, ThumbnailSize: 40, PreviewSize: 80, ExportTTL: time.Hour,
	}
	signer := sessions.NewSigner(cfg.SessionSecret)
	sm := sessions.NewManager(db, cfg.SessionTTL, false)
	queue := jobs.New(db, log)
	(&jobs.Handlers{DB: db, Store: store, Processor: images.NewGoProcessor(40, 80, 1), Queue: queue, ExportDir: filepath.Join(dir, "exports"), Log: log}).Register(queue)
	dl, err := downloads.New(db, store, queue, filepath.Join(dir, "exports"), cfg.ExportTTL, log)
	if err != nil {
		t.Fatal(err)
	}
	up := uploads.New(db, store, queue, uploads.Limits{MaxFileSize: cfg.UploadMaxFileSize, MaxImagesPerFolder: cfg.UploadMaxImagesPerFolder}, log)
	go queue.Run(ctx, 2)

	srv, err := apphttp.NewServer(apphttp.Deps{
		Config: cfg, DB: db, Store: store, Sessions: sm, Signer: signer,
		OIDC:    oidc.New(oidc.Config{}, db, sm, signer, false, log),
		Uploads: up, Downloads: dl, Queue: queue, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, h: srv.Handler(), db: db, storeDir: filepath.Join(dir, "photos")}

	// Sign in a user without going through OIDC.
	user, err := db.Q.UpsertUser(ctx, sqlc.UpsertUserParams{ID: "u1", OidcSub: "sub", Email: "a@b.c", Name: "Tester"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := sm.Create(ctx, rec, user.ID); err != nil {
		t.Fatal(err)
	}
	e.cookie = rec.Result().Cookies()[0]
	var csrf string
	if err := db.QueryRow("SELECT csrf_token FROM sessions").Scan(&csrf); err != nil {
		t.Fatal(err)
	}
	e.csrf = csrf
	return e
}

func (e *env) do(method, target string, body io.Reader, hdr map[string]string, cookies ...*nethttp.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) authed(method, target string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	hdr := map[string]string{"X-CSRF-Token": e.csrf, "HX-Request": "true"}
	if form != nil {
		body = strings.NewReader(form.Encode())
		hdr["Content-Type"] = "application/x-www-form-urlencoded"
	}
	return e.do(method, target, body, hdr, e.cookie)
}

func jpegFile(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 3), 100, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (e *env) upload(token string, nick *nethttp.Cookie, filename string, data []byte) (int, []map[string]any) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("files", filename)
	fw.Write(data)
	mw.Close()
	var cookies []*nethttp.Cookie
	if nick != nil {
		cookies = append(cookies, nick)
	}
	rec := e.do("POST", "/upload/"+token+"/images", &body, map[string]string{"Content-Type": mw.FormDataContentType()}, cookies...)
	var out struct{ Results []map[string]any }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Results
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)

	// Unauthenticated access is refused.
	if rec := e.do("GET", "/folders", nil, nil); rec.Code != nethttp.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/auth/login") {
		t.Fatalf("anonymous /folders: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// CSRF is enforced.
	if rec := e.do("POST", "/folders", strings.NewReader("name=x"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, e.cookie); rec.Code != nethttp.StatusForbidden {
		t.Fatalf("missing csrf: %d", rec.Code)
	}

	// Create a folder with a standard tag and an upload link.
	rec := e.authed("POST", "/folders", url.Values{"name": {"Summer Cup"}})
	if rec.Code != nethttp.StatusNoContent {
		t.Fatalf("create folder: %d %s", rec.Code, rec.Body)
	}
	folderPath := rec.Header().Get("HX-Redirect")
	folderID := strings.TrimPrefix(folderPath, "/folders/")
	if rec := e.authed("POST", folderPath+"/standard-tags", url.Values{"name": {"Football"}}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "football") {
		t.Fatalf("std tag: %d %s", rec.Code, rec.Body)
	}
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	m := regexp.MustCompile(`/upload/([A-Za-z0-9_-]+)"`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("create link: %d %s", rec.Code, rec.Body)
	}
	token := m[1]

	// Anonymous uploader: page, nickname, upload.
	if rec := e.do("GET", "/upload/"+token, nil, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Your nickname") {
		t.Fatalf("upload page: %d", rec.Code)
	}
	rec = e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Alice+B"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != nethttp.StatusSeeOther {
		t.Fatalf("nickname: %d", rec.Code)
	}
	nick := rec.Result().Cookies()[0]

	if code, _ := e.upload(token, nil, "a.jpg", jpegFile(t, 100, 50)); code != nethttp.StatusForbidden {
		t.Fatalf("upload without nickname: %d", code)
	}
	code, res := e.upload(token, nick, "../../IMG_1.jpg", jpegFile(t, 100, 50))
	if code != 200 || len(res) != 1 || res[0]["ok"] != true || res[0]["name"] != "IMG_1.jpg" {
		t.Fatalf("upload: %d %v", code, res)
	}
	if _, res := e.upload(token, nick, "IMG_1.jpg", jpegFile(t, 60, 60)); res[0]["ok"] != true {
		t.Fatalf("second upload: %v", res)
	}
	if _, res := e.upload(token, nick, "notes.jpg", []byte("this is not an image")); res[0]["ok"] == true || res[0]["error"] == "" {
		t.Fatalf("non-image accepted: %v", res)
	}
	if _, res := e.upload(token, nick, "big.jpg", append(jpegFile(t, 10, 10), make([]byte, 2<<20)...)); res[0]["ok"] == true {
		t.Fatalf("oversize accepted: %v", res)
	}
	if _, res := e.upload(token, nick, "c.jpg", jpegFile(t, 30, 30)); res[0]["ok"] != true {
		t.Fatalf("third upload: %v", res)
	}
	if _, res := e.upload(token, nick, "d.jpg", jpegFile(t, 30, 30)); res[0]["ok"] == true {
		t.Fatalf("folder limit not enforced: %v", res)
	}
	// Nothing was left behind by rejected files: 3 images -> 3 originals (+ derivatives later).
	imgs, err := e.db.Q.ListImagesInFolder(context.Background(), sqlc.ListImagesInFolderParams{FolderID: folderID, BeforeSeq: 1 << 60, PageSize: 10})
	if err != nil || len(imgs) != 3 {
		t.Fatalf("images: %v %v", len(imgs), err)
	}

	// Tags: standard + uploader tag, original filename preserved.
	first := imgs[len(imgs)-1]
	if first.OriginalFilename != "IMG_1.jpg" || first.UploaderNickname != "Alice B" {
		t.Fatalf("image metadata: %+v", first)
	}
	tags, _ := e.db.Q.ListImageTags(context.Background(), first.ID)
	var names []string
	for _, tg := range tags {
		names = append(names, tg.Name)
	}
	if strings.Join(names, ",") != "football,uploader/alice-b" {
		t.Fatalf("tags = %v", names)
	}

	// Derivatives are generated asynchronously.
	waitFor(t, "thumbnails", func() bool {
		var n int
		e.db.QueryRow("SELECT COUNT(*) FROM images WHERE thumbnail_ready=1 AND preview_ready=1").Scan(&n)
		return n == 3
	})

	// Authenticated image access works; anonymous does not.
	if rec := e.authed("GET", "/images/"+first.ID+"/thumbnail", nil); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumbnail: %d", rec.Code)
	}
	orig := e.authed("GET", "/images/"+first.ID+"/original", nil)
	if orig.Code != 200 || !bytes.Equal(orig.Body.Bytes()[:2], []byte{0xFF, 0xD8}) {
		t.Fatalf("original: %d", orig.Code)
	}
	for _, p := range []string{"thumbnail", "preview", "original"} {
		if rec := e.do("GET", "/images/"+first.ID+"/"+p, nil, nil); rec.Code == 200 {
			t.Fatalf("anonymous could read %s", p)
		}
		// The upload token must not grant access either.
		if rec := e.do("GET", "/images/"+first.ID+"/"+p, nil, nil, nick); rec.Code == 200 {
			t.Fatalf("nickname cookie granted %s", p)
		}
	}
	if rec := e.authed("GET", "/folders/"+folderID, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "IMG_1.jpg") {
		t.Fatalf("folder page: %d", rec.Code)
	}

	// Rate + tag.
	if rec := e.authed("POST", "/images/"+first.ID+"/rating", url.Values{"rating": {"4"}}); rec.Code != 200 {
		t.Fatalf("rate: %d %s", rec.Code, rec.Body)
	}
	if rec := e.authed("POST", "/images/"+first.ID+"/rating", url.Values{"rating": {"9"}}); rec.Code != 400 {
		t.Fatalf("bad rating: %d", rec.Code)
	}
	if rec := e.authed("POST", "/images/"+first.ID+"/tags", url.Values{"name": {"  GOAL "}}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "goal") {
		t.Fatalf("add tag: %d %s", rec.Code, rec.Body)
	}

	// Export everything.
	if rec := e.authed("POST", folderPath+"/downloads", url.Values{"all": {"1"}}); rec.Code != 200 {
		t.Fatalf("export: %d %s", rec.Code, rec.Body)
	}
	var exportID string
	waitFor(t, "export", func() bool {
		var status string
		if err := e.db.QueryRow("SELECT id, status FROM exports").Scan(&exportID, &status); err != nil {
			return false
		}
		return status == "ready"
	})
	zr := e.authed("GET", "/downloads/"+exportID, nil)
	if zr.Code != 200 {
		t.Fatalf("download: %d", zr.Code)
	}
	z, err := zip.NewReader(bytes.NewReader(zr.Body.Bytes()), int64(zr.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var zn []string
	xmp := ""
	for _, f := range z.File {
		zn = append(zn, f.Name)
		if f.Name == "Summer Cup/IMG_1.xmp" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			xmp = string(b)
		}
	}
	want := "Summer Cup/IMG_1.jpg,Summer Cup/IMG_1.xmp,Summer Cup/IMG_1_2.jpg,Summer Cup/IMG_1_2.xmp,Summer Cup/c.jpg,Summer Cup/c.xmp"
	if strings.Join(zn, ",") != want {
		t.Fatalf("zip entries:\n got %v\nwant %s", zn, want)
	}
	for _, s := range []string{`xmp:Rating="4"`, "<rdf:li>goal</rdf:li>", "<rdf:li>uploader/alice-b</rdf:li>", "<rdf:li>football</rdf:li>"} {
		if !strings.Contains(xmp, s) {
			t.Fatalf("xmp missing %q:\n%s", s, xmp)
		}
	}
	// The original in the ZIP is byte-identical to what was stored.
	for _, f := range z.File {
		if f.Name == "Summer Cup/IMG_1.jpg" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(b, orig.Body.Bytes()) {
				t.Fatal("zip original differs")
			}
		}
	}

	// Link lifecycle: regenerate invalidates the old token; expiry is explicit.
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"1"}})
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := e.do("GET", "/upload/"+token, nil, nil); rec.Code != 404 {
		t.Fatalf("old token still valid: %d", rec.Code)
	}
	newToken := regexp.MustCompile(`/upload/([A-Za-z0-9_-]+)"`).FindStringSubmatch(rec.Body.String())[1]
	e.db.Exec("UPDATE upload_links SET expires_at = '2000-01-01T00:00:00.000Z'")
	if rec := e.do("GET", "/upload/"+newToken, nil, nil); rec.Code != 410 || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("expired link: %d", rec.Code)
	}
	if code, _ := e.upload(newToken, nick, "x.jpg", jpegFile(t, 10, 10)); code != 410 {
		t.Fatalf("upload with expired link: %d", code)
	}
	if rec := e.authed("POST", folderPath+"/upload-link/extend", url.Values{"days": {"3"}}); rec.Code != 200 || strings.Contains(rec.Body.String(), "Expired") {
		t.Fatalf("extend: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/upload/"+newToken, nil, nil); rec.Code != 200 {
		t.Fatalf("extended link (same token) should work: %d", rec.Code)
	}
	e.authed("POST", folderPath+"/upload-link/revoke", nil)
	if rec := e.do("GET", "/upload/"+newToken, nil, nil); rec.Code != 404 {
		t.Fatalf("revoked link: %d", rec.Code)
	}

	// Delete the folder: inaccessible immediately, storage and rows removed by the job.
	if rec := e.authed("POST", folderPath+"/delete", nil); rec.Code != nethttp.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.authed("GET", folderPath, nil); rec.Code != 404 {
		t.Fatalf("deleted folder visible: %d", rec.Code)
	}
	if rec := e.authed("GET", "/images/"+first.ID+"/original", nil); rec.Code != 404 {
		t.Fatalf("image of deleted folder visible: %d", rec.Code)
	}
	waitFor(t, "folder cleanup", func() bool {
		var n int
		e.db.QueryRow("SELECT (SELECT COUNT(*) FROM folders) + (SELECT COUNT(*) FROM images) + (SELECT COUNT(*) FROM exports)").Scan(&n)
		return n == 0
	})
	if files, _ := filepath.Glob(filepath.Join(e.storeDir, "folders", "*")); len(files) != 0 {
		t.Fatalf("storage not cleaned: %v", files)
	}
}
