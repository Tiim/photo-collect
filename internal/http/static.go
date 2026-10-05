package http

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
)

// staticAssets serves web/static with cache busting. Templates link files with
// {{static "app.js"}}, which appends ?v=<content hash>: such a URL changes
// whenever the file does, so browsers may keep it forever. Other requests
// (e.g. Leaflet files loaded by map.js) carry the hash as ETag and must
// revalidate, which costs a 304 while the file is unchanged.
type staticAssets struct {
	hashes map[string]string // path below static/ -> content hash
	files  http.Handler
}

func newStaticAssets(fsys fs.FS) (*staticAssets, error) {
	a := &staticAssets{hashes: map[string]string{}, files: http.FileServerFS(fsys)}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a.hashes[p] = hex.EncodeToString(sum[:6])
		return nil
	})
	return a, err
}

// URL returns the versioned URL of a file below static/. An unknown name is an
// error, so a typo in a template fails rendering instead of linking a 404.
func (a *staticAssets) URL(name string) (string, error) {
	h, ok := a.hashes[name]
	if !ok {
		return "", fmt.Errorf("static: no file %q", name)
	}
	return "/static/" + name + "?v=" + h, nil
}

// ServeHTTP expects the /static/ prefix to be stripped already.
func (a *staticAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h, ok := a.hashes[r.URL.Path]; ok {
		// The file server answers If-None-Match with 304 based on this header.
		w.Header().Set("ETag", `"`+h+`"`)
		if r.URL.Query().Get("v") == h {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Unversioned, or a version from before a deploy.
			w.Header().Set("Cache-Control", "no-cache")
		}
	}
	a.files.ServeHTTP(w, r)
}
