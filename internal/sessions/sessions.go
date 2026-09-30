// Package sessions manages authenticated user sessions using opaque cookies.
package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
)

const CookieName = "session"

// Session is an authenticated user's session.
type Session struct {
	TokenHash string
	UserID    string
	Email     string
	Name      string
	CSRFToken string
}

// DisplayName is a human-friendly name for the user.
func (s *Session) DisplayName() string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Email != "":
		return s.Email
	}
	return "user"
}

type Manager struct {
	db     *database.DB
	ttl    time.Duration
	secure bool
}

// NewManager creates a Manager. secure controls the cookie Secure flag and
// should be true whenever the site is served over HTTPS.
func NewManager(db *database.DB, ttl time.Duration, secure bool) *Manager {
	return &Manager{db: db, ttl: ttl, secure: secure}
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Create starts a session for userID and sets the session cookie.
func (m *Manager) Create(ctx context.Context, w http.ResponseWriter, userID string) error {
	token := domain.NewToken()
	err := m.db.Q.CreateSession(ctx, sqlc.CreateSessionParams{
		TokenHash: hashToken(token),
		UserID:    userID,
		CsrfToken: domain.NewToken(),
		ExpiresAt: database.Time(time.Now().Add(m.ttl)),
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/",
		Expires:  time.Now().Add(m.ttl),
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Get returns the session for the request, or nil if not signed in.
func (m *Manager) Get(r *http.Request) (*Session, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	hash := hashToken(c.Value)
	now := time.Now()
	row, err := m.db.Q.GetSession(r.Context(), sqlc.GetSessionParams{TokenHash: hash, Now: database.Time(now)})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Sliding expiry: renew once half of the lifetime has passed.
	if exp, perr := database.ParseTime(row.ExpiresAt); perr == nil && exp.Sub(now) < m.ttl/2 {
		_ = m.db.Q.ExtendSession(r.Context(), sqlc.ExtendSessionParams{ExpiresAt: database.Time(now.Add(m.ttl)), TokenHash: hash})
	}
	return &Session{TokenHash: hash, UserID: row.UserID, Email: row.Email, Name: row.Name, CSRFToken: row.CsrfToken}, nil
}

// Delete removes the session presented with r from the database without
// touching cookies (used to rotate the session on login).
func (m *Manager) Delete(ctx context.Context, r *http.Request) error {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	return m.db.Q.DeleteSession(ctx, hashToken(c.Value))
}

// Destroy deletes the current session and clears the cookie.
func (m *Manager) Destroy(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		_ = m.db.Q.DeleteSession(r.Context(), hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode})
}

// PurgeExpired deletes expired sessions from the database.
func (m *Manager) PurgeExpired(ctx context.Context) error {
	return m.db.Q.DeleteExpiredSessions(ctx, database.Time(time.Now()))
}
