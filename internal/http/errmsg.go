package http

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/downloads"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/library"
	"github.com/tiim/photo-collect/internal/uploads"
)

// fail writes a plain-text error whose message is the catalog entry key in the
// request language.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, key string, data ...any) {
	http.Error(w, s.translator(r).T(key, data...), status)
}

// failErr writes err as a user-facing message when it is a known validation
// error, and a generic 400 message otherwise.
func (s *Server) failErr(w http.ResponseWriter, r *http.Request, status int, err error) {
	if msg, ok := s.errText(r, err); ok {
		http.Error(w, msg, status)
		return
	}
	s.fail(w, r, status, "err.bad_request")
}

// errText translates the validation errors that are shown to users. The bool is
// false for errors it does not know (those must not leak to the user).
func (s *Server) errText(r *http.Request, err error) (string, bool) {
	loc := s.translator(r)
	var ue *domain.UserError
	if errors.As(err, &ue) {
		args := make(map[string]any, len(ue.Args))
		for k, v := range ue.Args {
			if inner, ok := v.(error); ok {
				if txt, ok := s.errText(r, inner); ok {
					v = txt
				}
			}
			args[k] = v
		}
		if len(args) == 0 {
			return loc.T(ue.Key), true
		}
		return loc.T(ue.Key, args), true
	}
	var tl *downloads.TooLargeError
	switch {
	case errors.As(err, &tl):
		return loc.T("err.export_too_large", map[string]any{
			"Size": fmt.Sprintf("%.1f GiB", float64(tl.Size)/(1<<30)), "Max": fmt.Sprintf("%.1f GiB", float64(tl.Max)/(1<<30)),
		}), true
	case errors.Is(err, library.ErrTooMany):
		return loc.T("err.too_many_selected", map[string]any{"Max": library.MaxBatch}), true
	case errors.Is(err, library.ErrNothingSelected):
		return loc.T("err.select_one"), true
	case errors.Is(err, uploads.ErrTooLarge):
		return loc.T("err.upload.too_large", map[string]any{"MB": s.cfg.UploadMaxFileSize >> 20}), true
	case errors.Is(err, uploads.ErrFolderFull):
		return loc.T("err.upload.folder_full"), true
	case errors.Is(err, uploads.ErrFolderGone):
		return loc.T("err.upload.folder_gone"), true
	case errors.Is(err, uploads.ErrEmptyFile):
		return loc.T("err.upload.empty"), true
	case errors.Is(err, images.ErrAnimated):
		return loc.T("err.upload.animated"), true
	case errors.Is(err, images.ErrNotAnImage), errors.Is(err, images.ErrUnsupported):
		return loc.T("err.upload.not_image"), true
	case errors.Is(err, images.ErrTooManyPixels):
		return loc.T("err.upload.pixels"), true
	case errors.Is(err, images.ErrCorrupt):
		return loc.T("err.upload.corrupt"), true
	}
	return "", false
}
