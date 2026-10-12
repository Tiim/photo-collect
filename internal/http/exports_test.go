package http_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type exportList struct {
	Dir   string
	Files []struct {
		ID, Name, SHA256, URL string
		Size                  int64
		XMPName               string `json:"xmp_name"`
		XMP                   string
	}
}

func (e *env) exportList(target string, form url.Values) exportList {
	e.t.Helper()
	method := "GET"
	if form != nil {
		method = "POST"
	}
	rec := e.authed(method, target, form)
	if rec.Code != 200 {
		e.t.Fatalf("%s = %d %s", target, rec.Code, rec.Body)
	}
	var l exportList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		e.t.Fatal(err)
	}
	return l
}

// readZip checks the response length against Content-Length and returns the
// entries by name.
func (e *env) readZip(method, target string, form url.Values) map[string][]byte {
	e.t.Helper()
	rec := e.authed(method, target, form)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" {
		e.t.Fatalf("%s %s = %d %s", method, target, rec.Code, rec.Body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		e.t.Errorf("Content-Length %s, body has %d bytes", cl, rec.Body.Len())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			e.t.Fatal(err)
		}
		out[f.Name], err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			e.t.Fatalf("%s: %v", f.Name, err)
		}
	}
	return out
}

func TestExportStreamsZipAndListsFiles(t *testing.T) {
	e := setup(t)
	token, folderPath := e.newLink("Summer Party")
	rec := e.from("198.51.100.1:1", "POST", "/upload/"+token+"/nickname", strings.NewReader("nickname=Bob"), form)
	nick := rec.Result().Cookies()[0]
	photos := [][]byte{jpegFile(t, 50, 50), jpegFile(t, 60, 40)}
	for i, name := range []string{"a.jpg", "A.JPG"} {
		if code, res := e.upload(token, nick, name, photos[i]); code != 200 || res[0]["ok"] != true {
			t.Fatalf("upload: %d %v", code, res)
		}
	}

	list := e.exportList(folderPath+"/export.json?all=1", nil)
	if list.Dir != "Summer Party" || len(list.Files) != 2 {
		t.Fatalf("list: %+v", list)
	}
	if list.Files[0].Name != "a.jpg" || list.Files[1].Name != "A_2.JPG" || list.Files[1].XMPName != "A_2.xmp" {
		t.Errorf("names: %+v", list.Files)
	}
	for i, f := range list.Files {
		if f.Size != int64(len(photos[i])) || len(f.SHA256) != 64 {
			t.Errorf("%s: size %d sha256 %q", f.Name, f.Size, f.SHA256)
		}
		if rec := e.authed("GET", f.URL, nil); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), photos[i]) {
			t.Errorf("%s: original from %s = %d, not byte-identical", f.Name, f.URL, rec.Code)
		}
	}

	// The ZIP holds the same files: originals byte-identical, the same sidecars.
	entries := e.readZip("GET", folderPath+"/export.zip?all=1", nil)
	if len(entries) != 4 {
		t.Errorf("%d entries: %v", len(entries), entries)
	}
	for i, f := range list.Files {
		if !bytes.Equal(entries["Summer Party/"+f.Name], photos[i]) {
			t.Errorf("%s: original in ZIP not byte-identical", f.Name)
		}
		if string(entries["Summer Party/"+f.XMPName]) != f.XMP || !strings.Contains(f.XMP, "Bob") {
			t.Errorf("%s: sidecar in ZIP differs from the list:\n%s", f.Name, entries["Summer Party/"+f.XMPName])
		}
	}

	// A selection is posted (with the CSRF token) and keeps the names.
	sel := url.Values{"image": {list.Files[1].ID}}
	entries = e.readZip("POST", folderPath+"/export.zip", sel)
	if len(entries) != 2 || !bytes.Equal(entries["Summer Party/A_2.JPG"], photos[1]) {
		t.Errorf("selection: %v", entries)
	}
	if l := e.exportList(folderPath+"/export.json", sel); len(l.Files) != 1 || l.Files[0].Name != "A_2.JPG" {
		t.Errorf("selection list: %+v", l.Files)
	}
	if rec := e.do("POST", folderPath+"/export.zip", strings.NewReader(sel.Encode()), form, e.cookie); rec.Code != 403 {
		t.Errorf("without CSRF = %d", rec.Code)
	}
	if rec := e.authed("POST", folderPath+"/export.zip", url.Values{}); rec.Code != 400 {
		t.Errorf("empty selection = %d", rec.Code)
	}
	if rec := e.authed("HEAD", folderPath+"/export.zip?all=1", nil); rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD = %d, %d body bytes, Content-Length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	if rec := e.do("GET", folderPath+"/export.zip?all=1", nil, nil); rec.Code == 200 {
		t.Error("export without a session")
	}
}
