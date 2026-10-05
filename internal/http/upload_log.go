package http

import (
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/tiim/photo-collect/internal/clientip"
)

// Causes of an interrupted upload request, as logged in the "cause" field.
const (
	causeClientGone   = "client_disconnected" // the connection ended mid-body
	causeIdle         = "idle_timeout"        // the client sent nothing for chunkIdleTimeout
	causeSuperseded   = "superseded"          // the client sent the chunk again on a new request
	causeProxyTimeout = "proxy_timeout"       // ended at a whole minute: likely a reverse proxy read timeout
	causeOther        = "error"
)

// interruptCause guesses why reading an upload body failed after d.
func interruptCause(err error, d time.Duration) string {
	switch {
	case errors.Is(err, errChunkAborted):
		return causeSuperseded
	case errors.Is(err, os.ErrDeadlineExceeded):
		return causeIdle
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		if nearWholeMinute(d) {
			return causeProxyTimeout
		}
		return causeClientGone
	}
	return causeOther
}

// nearWholeMinute reports whether d is within a second of 60 s, 120 s, ...
// Proxies cut requests off at such round read timeouts (Traefik v3: 60 s),
// while dropped Wi-Fi connections end at random times.
func nearWholeMinute(d time.Duration) bool {
	m := math.Round(d.Minutes())
	return m >= 1 && math.Abs(d.Seconds()-m*60) < 1
}

// logInterrupted records an upload request that broke off, with enough detail
// to tell a flaky client connection from a proxy timeout.
func (s *Server) logInterrupted(r *http.Request, msg string, err error, start time.Time, received int64, attrs ...any) {
	d := time.Since(start)
	cause := interruptCause(err, d)
	attrs = append(attrs,
		"cause", cause,
		"received_bytes", received,
		"content_length", r.ContentLength,
		"duration_ms", d.Milliseconds(),
		"remote", clientip.String(r.Context()),
		"err", err,
	)
	if cause == causeProxyTimeout {
		attrs = append(attrs, "hint", "the request ended after a whole number of minutes; a reverse proxy read timeout probably cut it off (Traefik v3 entrypoints default to respondingTimeouts.readTimeout=60s)")
	}
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

func (c *countingReader) Close() error {
	if rc, ok := c.r.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}
