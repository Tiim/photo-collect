// Package oidc implements the OpenID Connect login flow (authorization code + PKCE).
package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
	"github.com/tiim/photo-collect/internal/sessions"
)

const flowCookie = "oidc_flow"

type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

type Handler struct {
	cfg      Config
	db       *database.DB
	sessions *sessions.Manager
	signer   *sessions.Signer
	secure   bool
	log      *slog.Logger

	mu       sync.Mutex
	provider *gooidc.Provider
}

func New(cfg Config, db *database.DB, sm *sessions.Manager, signer *sessions.Signer, secure bool, log *slog.Logger) *Handler {
	return &Handler{cfg: cfg, db: db, sessions: sm, signer: signer, secure: secure, log: log}
}

// getProvider performs OIDC discovery lazily so the app can start while the
// identity provider is still unavailable.
func (h *Handler) getProvider(ctx context.Context) (*gooidc.Provider, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.provider != nil {
		return h.provider, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := gooidc.NewProvider(ctx, h.cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	h.provider = p
	return p, nil
}

func (h *Handler) oauth(p *gooidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     h.cfg.ClientID,
		ClientSecret: h.cfg.ClientSecret,
		RedirectURL:  h.cfg.RedirectURL,
		Endpoint:     p.Endpoint(),
		Scopes:       []string{gooidc.ScopeOpenID, "profile", "email"},
	}
}

// Login starts the authorization code flow.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	p, err := h.getProvider(r.Context())
	if err != nil {
		h.log.Error("oidc discovery failed", "err", err)
		http.Error(w, "Login is temporarily unavailable", http.StatusBadGateway)
		return
	}
	state, nonce, verifier := domain.NewToken(), domain.NewToken(), oauth2.GenerateVerifier()
	next := SafeRedirect(r.URL.Query().Get("next"))

	http.SetCookie(w, &http.Cookie{
		Name:     flowCookie,
		Value:    h.signer.Sign(flowCookie, strings.Join([]string{state, nonce, verifier, next}, "\n")),
		Path:     "/auth/",
		MaxAge:   600,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
	url := h.oauth(p).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), gooidc.Nonce(nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the flow, creates the user's session and redirects.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string, err error) {
		h.log.Warn("authentication failed", "reason", msg, "err", err, "remote", r.RemoteAddr)
		http.Error(w, msg, status)
	}
	c, err := r.Cookie(flowCookie)
	if err != nil {
		fail(http.StatusBadRequest, "Login session expired, please try again", err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: "/auth/", MaxAge: -1})
	payload, err := h.signer.Verify(flowCookie, c.Value)
	parts := strings.Split(payload, "\n")
	if err != nil || len(parts) != 4 {
		fail(http.StatusBadRequest, "Invalid login state", err)
		return
	}
	state, nonce, verifier, next := parts[0], parts[1], parts[2], parts[3]
	if r.URL.Query().Get("state") != state {
		fail(http.StatusBadRequest, "Invalid login state", errors.New("state mismatch"))
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		fail(http.StatusForbidden, "Login was denied", fmt.Errorf("provider error: %s", e))
		return
	}

	p, err := h.getProvider(r.Context())
	if err != nil {
		fail(http.StatusBadGateway, "Login is temporarily unavailable", err)
		return
	}
	tok, err := h.oauth(p).Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		fail(http.StatusUnauthorized, "Login failed", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idToken, err := p.Verifier(&gooidc.Config{ClientID: h.cfg.ClientID}).Verify(r.Context(), rawID)
	if err != nil {
		fail(http.StatusUnauthorized, "Login failed", err)
		return
	}
	if idToken.Nonce != nonce {
		fail(http.StatusUnauthorized, "Login failed", errors.New("nonce mismatch"))
		return
	}
	var claims struct {
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		fail(http.StatusUnauthorized, "Login failed", err)
		return
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}

	user, err := h.db.Q.UpsertUser(r.Context(), sqlc.UpsertUserParams{
		ID: domain.NewID(), OidcSub: idToken.Subject, Email: claims.Email, Name: name,
	})
	if err != nil {
		h.log.Error("upsert user", "err", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.sessions.Create(r.Context(), w, user.ID); err != nil {
		h.log.Error("create session", "err", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.log.Info("user logged in", "user_id", user.ID)
	http.Redirect(w, r, SafeRedirect(next), http.StatusSeeOther)
}

// SafeRedirect only allows local absolute paths, preventing open redirects.
func SafeRedirect(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}
