package http_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tiim/photo-collect/internal/config"
	apphttp "github.com/tiim/photo-collect/internal/http"
)

// from performs a request as if it came from the given peer address.
func (e *env) from(remote, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// newLink creates a folder and an upload link, returning the token.
func (e *env) newLink(name string) (token, folderPath string) {
	e.t.Helper()
	rec := e.authed("POST", "/folders", url.Values{"name": {name}})
	folderPath = rec.Header().Get("HX-Redirect")
	rec = e.authed("POST", folderPath+"/upload-link", url.Values{"days": {"2"}})
	m := regexp.MustCompile(`/upload/([A-Za-z0-9_-]+)"`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		e.t.Fatalf("no upload link: %d %s", rec.Code, rec.Body)
	}
	return m[1], folderPath
}

var form = map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

func TestUploadRateLimitPerIP(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.RateUploadPerIP = 3 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, _ := e.newLink("A")
	for i := 0; i < 3; i++ {
		if rec := e.from("198.51.100.1:1000", "GET", "/upload/"+token, nil, nil); rec.Code != 200 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := e.from("198.51.100.1:1001", "GET", "/upload/"+token, nil, nil)
	if rec.Code != nethttp.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("4th request: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := e.from("198.51.100.2:1", "GET", "/upload/"+token, nil, nil); rec.Code != 200 {
		t.Errorf("another client must not be limited: %d", rec.Code)
	}
	// Signed-in pages are not throttled by the upload limit.
	if rec := e.authed("GET", "/folders", nil); rec.Code != 200 {
		t.Errorf("/folders: %d", rec.Code)
	}
}

func TestUploadRateLimitGroupsIPv6By64(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.RateUploadPerIP = 1 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, _ := e.newLink("A")
	if rec := e.from("[2001:db8:1:2::1]:1", "GET", "/upload/"+token, nil, nil); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := e.from("[2001:db8:1:2:aaaa::9]:1", "GET", "/upload/"+token, nil, nil); rec.Code != nethttp.StatusTooManyRequests {
		t.Errorf("same /64 must share a bucket: %d", rec.Code)
	}
	if rec := e.from("[2001:db8:1:3::1]:1", "GET", "/upload/"+token, nil, nil); rec.Code != 200 {
		t.Errorf("different /64 must not: %d", rec.Code)
	}
}

func TestUploadRateLimitPerLink(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.RateUploadPerLink = 2 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t1, _ := e.newLink("A")
	t2, _ := e.newLink("B")
	for i, ip := range []string{"198.51.100.1:1", "198.51.100.2:1"} {
		if rec := e.from(ip, "GET", "/upload/"+t1, nil, nil); rec.Code != 200 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if rec := e.from("198.51.100.3:1", "GET", "/upload/"+t1, nil, nil); rec.Code != nethttp.StatusTooManyRequests {
		t.Errorf("link limit is shared across IPs: %d", rec.Code)
	}
	if rec := e.from("198.51.100.3:1", "GET", "/upload/"+t2, nil, nil); rec.Code != 200 {
		t.Errorf("other link unaffected: %d", rec.Code)
	}
}

func TestNicknameRateLimit(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.RateNicknamePerIP = 2 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, _ := e.newLink("A")
	post := func() int {
		return e.from("198.51.100.1:1", "POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), form).Code
	}
	if a, b := post(), post(); a != nethttp.StatusSeeOther || b != nethttp.StatusSeeOther {
		t.Fatalf("within limit: %d %d", a, b)
	}
	if c := post(); c != nethttp.StatusTooManyRequests {
		t.Errorf("3rd nickname change: %d", c)
	}
}

func TestAuthRateLimit(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.RateAuthPerIP = 2 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	get := func(ip, path string) int { return e.from(ip, "GET", path, nil, nil).Code }
	// Login and callback share one per-IP budget.
	if a, b := get("203.0.113.5:9", "/auth/login"), get("203.0.113.5:9", "/auth/callback"); a == nethttp.StatusTooManyRequests || b == nethttp.StatusTooManyRequests {
		t.Fatalf("within limit: %d %d", a, b)
	}
	for _, path := range []string{"/auth/login", "/auth/callback"} {
		if code := get("203.0.113.5:9", path); code != nethttp.StatusTooManyRequests {
			t.Errorf("%s after limit: %d", path, code)
		}
	}
	if code := get("203.0.113.6:9", "/auth/login"); code == nethttp.StatusTooManyRequests {
		t.Error("other IP limited")
	}
}

func TestRateLimitLogNeverContainsToken(t *testing.T) {
	var buf bytes.Buffer
	e := setupWith(t, func(c *config.Config) { c.RateUploadPerIP = 1 }, slog.New(slog.NewTextHandler(&buf, nil)))
	token, _ := e.newLink("A")
	e.from("198.51.100.1:1", "GET", "/upload/"+token, nil, nil)
	buf.Reset()
	if rec := e.from("198.51.100.1:1", "GET", "/upload/"+token, nil, nil); rec.Code != nethttp.StatusTooManyRequests {
		t.Fatalf("code %d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "rate limited") || !strings.Contains(out, "limit=upload-ip") {
		t.Errorf("limiter hit not logged:\n%s", out)
	}
	if strings.Contains(out, token) {
		t.Errorf("token leaked into log:\n%s", out)
	}
}

func TestConcurrentUploadsAreBounded(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.UploadMaxConcurrent = 1 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, _ := e.newLink("A")
	rec := e.from("198.51.100.1:1", "POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), form)
	nick := rec.Result().Cookies()[0]

	// The bytes of a file arrive while another upload is being processed.
	data := jpegFile(t, 50, 50)
	start := e.do("POST", "/upload/"+token+"/chunked", strings.NewReader(`{"name":"a.jpg","size":`+strconv.Itoa(len(data))+`}`), nil, nick)
	var st struct{ ID string }
	json.Unmarshal(start.Body.Bytes(), &st)
	base := "/upload/" + token + "/chunked/" + st.ID
	release := apphttp.HoldIngestSlots(e.srv)
	if rec := e.do("PUT", base, bytes.NewReader(data), map[string]string{"Upload-Offset": "0"}); rec.Code != 200 {
		t.Fatalf("chunks are accepted while busy: %d %s", rec.Code, rec.Body)
	}

	// Processing it has to wait for a free slot.
	rec = e.do("POST", base+"/complete", nil, nil)
	if rec.Code != nethttp.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("complete while busy: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	release()
	rec = e.do("POST", base+"/complete", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("complete after the slot is free (bytes kept): %d %s", rec.Code, rec.Body)
	}
}

func TestExportTooLargeIsRefusedWithMessage(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.ExportMaxBytes = 10 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, folderPath := e.newLink("A")
	rec := e.from("198.51.100.1:1", "POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), form)
	if code, res := e.upload(token, rec.Result().Cookies()[0], "a.jpg", jpegFile(t, 50, 50)); code != 200 || res[0]["ok"] != true {
		t.Fatalf("upload: %d %v", code, res)
	}
	rec = e.authed("POST", folderPath+"/downloads", url.Values{"all": {"1"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Export is too large") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM exports").Scan(&n)
	if n != 0 {
		t.Errorf("%d export rows created", n)
	}
}
