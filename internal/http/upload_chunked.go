package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tiim/photo-collect/internal/clientip"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/uploads"
)

// Resumable uploads for flaky connections. The upload page:
//
//  1. POST /upload/{token}/chunked {"name","size","sha256"} -> 201 {"id","chunk_size","offset"}
//     "sha256" (lowercase hex) is optional; if the folder already has a file
//     with that hash the answer is 200 {"results":[{"name","ok":true,"duplicate":true}]}
//     and nothing has to be sent.
//  2. PUT  /upload/{token}/chunked/{id} with header Upload-Offset and up to
//     chunk_size bytes -> 200 {"offset"}; on 409 it continues at the returned
//     offset, so a request cut off mid-way only costs what was not received.
//  3. POST /upload/{token}/chunked/{id}/complete -> {"results":[{"name","ok","error"}]};
//     asking again returns the same outcome.
//
// A 404 with {"error"} on steps 2 and 3 means the server no longer knows the
// upload (restart, or abandoned too long) and the file has to be sent again.

// chunkIdleTimeout ends a chunk request whose client stopped sending, so a
// half-open connection does not hold the upload until TCP gives up.
const chunkIdleTimeout = 30 * time.Second

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type chunkState struct {
	ID        string `json:"id,omitempty"`
	ChunkSize int64  `json:"chunk_size,omitempty"`
	Offset    int64  `json:"offset"`
	Error     string `json:"error,omitempty"`
}

func writeChunkState(w http.ResponseWriter, status int, st chunkState) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(st)
}

func (s *Server) chunkedStart(w http.ResponseWriter, r *http.Request) {
	l, ok := s.uploadLink(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "err.forbidden")
		return
	}
	nick := s.nickname(r)
	if nick == "" {
		writeJSON(w, http.StatusForbidden, []uploadResult{{Error: s.translator(r).T("err.upload.nickname_first")}})
		return
	}
	var req struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		s.fail(w, r, http.StatusBadRequest, "err.bad_request")
		return
	}
	name := domain.SafeFilename(req.Name)
	reject := func(err error) {
		writeJSON(w, http.StatusBadRequest, []uploadResult{{Name: name, Error: s.uploadErrorMessage(r, err, l.FolderID, name)}})
	}
	switch {
	case req.Size <= 0:
		reject(uploads.ErrEmptyFile)
		return
	case req.Size > s.cfg.UploadMaxFileSize:
		reject(uploads.ErrTooLarge)
		return
	}
	if req.SHA256 != "" {
		if !sha256Hex.MatchString(req.SHA256) {
			s.fail(w, r, http.StatusBadRequest, "err.bad_request")
			return
		}
		// Checked before the capacity so a full folder still recognises
		// files it already has.
		dup, err := s.uploads.HasImage(r.Context(), l.FolderID, req.SHA256)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if dup {
			s.log.Info("upload skipped: already in folder", "folder_id", l.FolderID, "size", req.Size, "remote", clientip.String(r.Context()))
			writeJSON(w, http.StatusOK, []uploadResult{{Name: name, OK: true, Duplicate: true}})
			return
		}
	}
	if err := s.uploads.CheckCapacity(r.Context(), l.FolderID); err != nil {
		reject(err)
		return
	}
	p, err := s.chunks.Start(l.Token, l.FolderID, nick, name, req.Size)
	if errors.Is(err, uploads.ErrTooManyPending) {
		open, limit := s.chunks.Open()
		s.log.Info("upload rejected: too many pending uploads", "route", routeLabel(r), "remote", clientip.String(r.Context()),
			"pending", open, "limit", limit)
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusServiceUnavailable, []uploadResult{{Name: name, Error: s.translator(r).T("err.upload.busy")}})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("chunked upload started", "folder_id", l.FolderID, "upload", p.LogID(), "size", req.Size,
		"chunk_size", s.cfg.UploadChunkSize, "remote", clientip.String(r.Context()))
	writeChunkState(w, http.StatusCreated, chunkState{ID: p.ID, ChunkSize: s.cfg.UploadChunkSize})
}

// chunkedPut is not rate limited: the requests only reach work for an upload
// that was started through the limited start route, and a large file needs
// many of them.
func (s *Server) chunkedPut(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "err.forbidden")
		return
	}
	start := time.Now()
	p, err := s.chunks.Get(r.PathValue("id"), r.PathValue("token"))
	if err != nil {
		// Expected after a restart or once an abandoned upload was discarded;
		// the page then sends the file again.
		s.log.Info("upload chunk for unknown upload", "remote", clientip.String(r.Context()))
		writeChunkState(w, http.StatusNotFound, chunkState{Error: s.translator(r).T("err.upload.interrupted")})
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		writeChunkState(w, http.StatusBadRequest, chunkState{Offset: p.Offset(), Error: s.translator(r).T("err.bad_request")})
		return
	}
	rc := http.NewResponseController(w)
	counted := &countingReader{r: http.MaxBytesReader(w, r.Body, s.cfg.UploadChunkSize)}
	body := &idleReader{r: counted, rc: rc}
	n, err := s.chunks.Write(p, offset, body, body.abort)
	_ = rc.SetReadDeadline(time.Time{})
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		writeChunkState(w, http.StatusOK, chunkState{Offset: n})
	case errors.Is(err, uploads.ErrOffsetMismatch):
		s.log.Info("upload chunk offset mismatch", "folder_id", p.FolderID, "upload", p.LogID(),
			"client_offset", offset, "server_offset", n, "size", p.Size)
		writeChunkState(w, http.StatusConflict, chunkState{Offset: n})
	case errors.Is(err, uploads.ErrUploadNotFound):
		writeChunkState(w, http.StatusNotFound, chunkState{Error: s.translator(r).T("err.upload.interrupted")})
	case errors.As(err, &tooBig), errors.Is(err, uploads.ErrTooLarge):
		writeChunkState(w, http.StatusRequestEntityTooLarge, chunkState{Offset: n, Error: s.translator(r).T("err.upload.too_large", map[string]any{"MB": s.cfg.UploadMaxFileSize >> 20})})
	default:
		// The client resumes at the offset.
		s.logIncompleteChunk(r, "upload chunk incomplete", err, start, counted.n,
			"folder_id", p.FolderID, "upload", p.LogID(), "chunk_offset", offset, "resume_offset", n, "size", p.Size)
		writeChunkState(w, http.StatusBadRequest, chunkState{Offset: n, Error: s.translator(r).T("err.upload.interrupted")})
	}
}

func (s *Server) chunkedComplete(w http.ResponseWriter, r *http.Request) {
	l, ok := s.uploadLink(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "err.forbidden")
		return
	}
	p, err := s.chunks.Get(r.PathValue("id"), l.Token)
	if err != nil {
		writeChunkState(w, http.StatusNotFound, chunkState{Error: s.translator(r).T("err.upload.interrupted")})
		return
	}
	release, ok := s.acquireIngestSlot(w, r)
	if !ok {
		return
	}
	defer release()
	res := uploadResult{Name: p.Filename}
	// The client may drop the connection while the file is processed; finish
	// anyway so its retry gets the result instead of a second ingest.
	ctx := context.WithoutCancel(r.Context())
	err = s.chunks.Complete(p, func(f io.Reader, st uploads.Stats) error {
		_, err := s.uploads.Ingest(ctx, p.FolderID, p.Nickname, p.Filename, f)
		if err == nil {
			s.log.Info("image uploaded", "folder_id", p.FolderID, "chunked", true, "upload", p.LogID(),
				"size", p.Size, "chunks", st.Chunks, "incomplete_chunks", st.Incomplete, "offset_mismatches", st.OffsetMismatches,
				"duration_ms", time.Since(st.Started).Milliseconds())
		}
		return err
	})
	switch {
	case errors.Is(err, uploads.ErrIncomplete):
		writeChunkState(w, http.StatusConflict, chunkState{Offset: p.Offset()})
		return
	case errors.Is(err, uploads.ErrUploadNotFound):
		writeChunkState(w, http.StatusNotFound, chunkState{Error: s.translator(r).T("err.upload.interrupted")})
		return
	case err != nil:
		res.Error = s.uploadErrorMessage(r, err, p.FolderID, p.Filename)
	default:
		res.OK = true
	}
	writeJSON(w, http.StatusOK, []uploadResult{res})
}

var errChunkAborted = errors.New("chunk request superseded")

// idleReader fails a read once the client has sent nothing for
// chunkIdleTimeout, or once abort was called.
type idleReader struct {
	r       io.Reader
	rc      *http.ResponseController
	aborted atomic.Bool
}

func (ir *idleReader) Read(b []byte) (int, error) {
	if ir.aborted.Load() {
		return 0, errChunkAborted
	}
	_ = ir.rc.SetReadDeadline(time.Now().Add(chunkIdleTimeout))
	return ir.r.Read(b)
}

// abort unblocks a pending Read and fails all later ones.
func (ir *idleReader) abort() {
	ir.aborted.Store(true)
	_ = ir.rc.SetReadDeadline(time.Now())
}
