package http

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// clockPage is the public landing page: a clock (text and QR code) that people
// photograph with each of their cameras. Signed-in users get a link to their
// folders, everyone else a sign-in button.
func (s *Server) clockPage(w http.ResponseWriter, r *http.Request) {
	sess, err := s.sessions.Get(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if sess != nil {
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess))
	}
	s.page(w, r, http.StatusOK, "clock", "Camera clock", nil)
}

// serverTime tells the clock page the server's time, so a visitor whose own
// device clock is wrong still shows the correct time.
func (s *Server) serverTime(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"ms":%d}`+"\n", time.Now().UnixMilli())
}
