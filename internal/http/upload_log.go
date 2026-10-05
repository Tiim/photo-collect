package http

import (
	"io"
	"net/http"
	"time"

	"github.com/tiim/photo-collect/internal/clientip"
)

// logIncompleteChunk records a chunk request whose body could not be read in
// full. It logs only what the server observed; err is the read error as
// returned (e.g. unexpected EOF, an i/o timeout from the idle deadline, or
// errChunkAborted when a newer request for the same upload arrived).
func (s *Server) logIncompleteChunk(r *http.Request, msg string, err error, start time.Time, received int64, attrs ...any) {
	attrs = append(attrs,
		"received_bytes", received,
		"content_length", r.ContentLength,
		"duration_ms", time.Since(start).Milliseconds(),
		"remote", clientip.String(r.Context()),
		"err", err,
	)
	s.log.Info(msg, attrs...)
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}
