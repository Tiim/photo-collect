package http_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/config"
)

// chunkedEnv sets up a server with 1000-byte chunks; logs go to logs, if not nil.
func chunkedEnv(t *testing.T, logs io.Writer) (*env, string, *nethttp.Cookie) {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	e := setupWith(t, func(c *config.Config) { c.UploadChunkSize = 1000 }, slog.New(slog.NewTextHandler(logs, nil)))
	token, _ := e.newLink("A")
	rec := e.do("POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), form)
	return e, token, rec.Result().Cookies()[0]
}

type chunkResp struct {
	ID        string `json:"id"`
	ChunkSize int64  `json:"chunk_size"`
	Offset    int64  `json:"offset"`
	Error     string `json:"error"`
	Results   []map[string]any
}

func decodeChunk(t *testing.T, rec *httptest.ResponseRecorder) chunkResp {
	t.Helper()
	var out chunkResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body)
	}
	return out
}

func (e *env) chunkStart(token string, nick *nethttp.Cookie, name string, size int) *httptest.ResponseRecorder {
	body := `{"name":` + strconv.Quote(name) + `,"size":` + strconv.Itoa(size) + `}`
	var cookies []*nethttp.Cookie
	if nick != nil {
		cookies = append(cookies, nick)
	}
	return e.do("POST", "/upload/"+token+"/chunked", strings.NewReader(body), map[string]string{"Content-Type": "application/json"}, cookies...)
}

func (e *env) chunkPut(token, id string, offset int, body io.Reader) *httptest.ResponseRecorder {
	return e.do("PUT", "/upload/"+token+"/chunked/"+id, body, map[string]string{"Upload-Offset": strconv.Itoa(offset)})
}

// failingReader yields data and then fails like a dropped connection.
type failingReader struct{ r io.Reader }

func (f *failingReader) Read(b []byte) (int, error) {
	n, err := f.r.Read(b)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestChunkedUploadResumesAfterInterruption(t *testing.T) {
	var logs bytes.Buffer
	e, token, nick := chunkedEnv(t, &logs)
	data := jpegFile(t, 300, 300)
	if len(data) < 3000 {
		t.Fatalf("test image too small for several chunks: %d", len(data))
	}

	rec := e.chunkStart(token, nick, "../IMG_7.jpg", len(data))
	st := decodeChunk(t, rec)
	if rec.Code != nethttp.StatusCreated || st.ID == "" || st.ChunkSize != 1000 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}

	// The first chunk arrives completely.
	rec = e.chunkPut(token, st.ID, 0, bytes.NewReader(data[:1000]))
	if rec.Code != 200 || decodeChunk(t, rec).Offset != 1000 {
		t.Fatalf("chunk 1: %d %s", rec.Code, rec.Body)
	}
	// The second one is cut off after 400 bytes: they are kept.
	rec = e.chunkPut(token, st.ID, 1000, &failingReader{bytes.NewReader(data[1000:1400])})
	if rec.Code == 200 || decodeChunk(t, rec).Offset != 1400 {
		t.Fatalf("interrupted chunk: %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`msg="upload chunk interrupted"`, "cause=client_disconnected", "received_bytes=400", "chunk_offset=1000", "resume_offset=1400", "duration_ms="} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("interruption log lacks %s:\n%s", want, logs.String())
		}
	}
	// The client did not see that response and resends the whole chunk: the
	// server tells it where to continue.
	rec = e.chunkPut(token, st.ID, 1000, bytes.NewReader(data[1000:2000]))
	if rec.Code != nethttp.StatusConflict || decodeChunk(t, rec).Offset != 1400 {
		t.Fatalf("stale offset: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "client_offset=1000 server_offset=1400") {
		t.Errorf("resume not logged:\n%s", logs.String())
	}
	// Completing early is refused with the current offset.
	rec = e.do("POST", "/upload/"+token+"/chunked/"+st.ID+"/complete", nil, nil)
	if rec.Code != nethttp.StatusConflict || decodeChunk(t, rec).Offset != 1400 {
		t.Fatalf("early complete: %d %s", rec.Code, rec.Body)
	}
	for off := 1400; off < len(data); off += 1000 {
		end := min(off+1000, len(data))
		if rec := e.chunkPut(token, st.ID, off, bytes.NewReader(data[off:end])); rec.Code != 200 {
			t.Fatalf("chunk at %d: %d %s", off, rec.Code, rec.Body)
		}
	}

	rec = e.do("POST", "/upload/"+token+"/chunked/"+st.ID+"/complete", nil, nil)
	res := decodeChunk(t, rec).Results
	if rec.Code != 200 || len(res) != 1 || res[0]["ok"] != true || res[0]["name"] != "IMG_7.jpg" {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body)
	}
	chunks := (len(data)-1400+999)/1000 + 1
	for _, want := range []string{`msg="image uploaded"`, "chunked=true", fmt.Sprintf("chunks=%d", chunks), "interrupted=1", "resumed=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("upload log lacks %s:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), st.ID) || strings.Contains(logs.String(), token) {
		t.Errorf("upload ID or link token leaked into the log:\n%s", logs.String())
	}
	// A retried complete (lost response) gets the same answer without a second image.
	rec = e.do("POST", "/upload/"+token+"/chunked/"+st.ID+"/complete", nil, nil)
	if res := decodeChunk(t, rec).Results; rec.Code != 200 || res[0]["ok"] != true {
		t.Fatalf("repeated complete: %d %s", rec.Code, rec.Body)
	}

	var n int
	var name, nickname string
	var size int
	if err := e.db.QueryRow("SELECT count(*), max(original_filename), max(uploader_nickname), max(size_bytes) FROM images").Scan(&n, &name, &nickname, &size); err != nil {
		t.Fatal(err)
	}
	if n != 1 || name != "IMG_7.jpg" || nickname != "Bob" || size != len(data) {
		t.Errorf("stored image: n=%d name=%q nick=%q size=%d", n, name, nickname, size)
	}
}

func TestChunkedUploadRejections(t *testing.T) {
	e, token, nick := chunkedEnv(t, nil)
	data := jpegFile(t, 20, 20)

	if rec := e.chunkStart(token, nil, "a.jpg", len(data)); rec.Code != nethttp.StatusForbidden {
		t.Errorf("without nickname: %d", rec.Code)
	}
	rec := e.chunkStart(token, nick, "a.jpg", 2<<20)
	if res := decodeChunk(t, rec).Results; rec.Code != nethttp.StatusBadRequest || !strings.Contains(res[0]["error"].(string), "too large") {
		t.Errorf("too large: %d %s", rec.Code, rec.Body)
	}
	if rec := e.chunkStart(token, nick, "a.jpg", 0); rec.Code != nethttp.StatusBadRequest {
		t.Errorf("empty: %d", rec.Code)
	}
	if rec := e.chunkPut(token, "nope", 0, bytes.NewReader(data)); rec.Code != nethttp.StatusNotFound {
		t.Errorf("unknown upload: %d", rec.Code)
	}

	st := decodeChunk(t, e.chunkStart(token, nick, "a.jpg", 10))
	// An upload belongs to its link.
	other, _ := e.newLink("B")
	if rec := e.chunkPut(other, st.ID, 0, strings.NewReader("0123456789")); rec.Code != nethttp.StatusNotFound {
		t.Errorf("other link: %d", rec.Code)
	}
	// More bytes than declared, or than one chunk, are refused.
	if rec := e.chunkPut(token, st.ID, 0, strings.NewReader("0123456789x")); rec.Code != nethttp.StatusRequestEntityTooLarge {
		t.Errorf("beyond declared size: %d %s", rec.Code, rec.Body)
	}
	big := decodeChunk(t, e.chunkStart(token, nick, "b.jpg", 5000))
	if rec := e.chunkPut(token, big.ID, 0, bytes.NewReader(make([]byte, 1001))); rec.Code != nethttp.StatusRequestEntityTooLarge {
		t.Errorf("beyond chunk size: %d %s", rec.Code, rec.Body)
	}
	// A cross-site request is refused.
	if rec := e.do("PUT", "/upload/"+token+"/chunked/"+big.ID, strings.NewReader("x"), map[string]string{"Upload-Offset": "0", "Origin": "https://evil.test"}); rec.Code != nethttp.StatusForbidden {
		t.Errorf("cross-origin: %d", rec.Code)
	}

	// Content that is not an image is reported on complete.
	st = decodeChunk(t, e.chunkStart(token, nick, "notes.jpg", 10))
	e.chunkPut(token, st.ID, 0, strings.NewReader("0123456789"))
	rec = e.do("POST", "/upload/"+token+"/chunked/"+st.ID+"/complete", nil, nil)
	if res := decodeChunk(t, rec).Results; rec.Code != 200 || res[0]["ok"] == true || res[0]["error"] == "" {
		t.Errorf("not an image: %d %s", rec.Code, rec.Body)
	}
}

func TestChunkedUploadPendingLimit(t *testing.T) {
	e, token, nick := chunkedEnv(t, nil) // the test server allows 4 pending uploads
	for i := 0; i < 4; i++ {
		if rec := e.chunkStart(token, nick, "a.jpg", 100); rec.Code != nethttp.StatusCreated {
			t.Fatalf("start %d: %d", i, rec.Code)
		}
	}
	rec := e.chunkStart(token, nick, "a.jpg", 100)
	if rec.Code != nethttp.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d %v", rec.Code, rec.Header())
	}
}

// A chunk request stuck on a dead connection is cut off when the client sends
// the next one, instead of blocking the upload until TCP gives up.
func TestChunkedUploadSupersedesStuckRequest(t *testing.T) {
	e, token, nick := chunkedEnv(t, nil)
	st := decodeChunk(t, e.chunkStart(token, nick, "a.jpg", 10))
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	url := srv.URL + "/upload/" + token + "/chunked/" + st.ID

	// The first request announces 10 bytes but sends only 5, then stalls.
	pr, pw := io.Pipe()
	defer pw.Close()
	req, _ := nethttp.NewRequest("PUT", url, pr)
	req.ContentLength = 10
	req.Header.Set("Upload-Offset", "0")
	go func() {
		if resp, err := nethttp.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	pw.Write([]byte("01234"))

	client := &nethttp.Client{Timeout: 10 * time.Second}
	put := func(offset int, body string) (int, chunkResp) {
		req, _ := nethttp.NewRequest("PUT", url, strings.NewReader(body))
		req.Header.Set("Upload-Offset", strconv.Itoa(offset))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out chunkResp
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	// The client gave up on the first request and continues after the 5 bytes
	// (until the server has received them it answers 409 at offset 0). This
	// only succeeds once the stuck request has been cut off and released the upload.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, out := put(5, "56789")
		if code == 200 && out.Offset == 10 {
			break
		}
		if code != nethttp.StatusConflict || out.Offset != 0 || time.Now().After(deadline) {
			t.Fatalf("resume: %d %+v", code, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec := e.do("POST", "/upload/"+token+"/chunked/"+st.ID+"/complete", nil, nil)
	if res := decodeChunk(t, rec).Results; rec.Code != 200 || len(res) != 1 {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body)
	}
}
