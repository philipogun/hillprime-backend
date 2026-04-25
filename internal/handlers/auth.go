// Package handlers — auth endpoints.
//
// These replace everything that would have gone to supabase.auth. The API
// is minimal by design:
//
//	POST /auth/magic-link     request a sign-in email
//	GET  /auth/callback       exchange the link for a session cookie
//	POST /auth/logout         revoke the current session
//	GET  /auth/me             return the current user
//
// No refresh token juggling on the client: the session cookie IS the
// credential.
package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strings"

	"github.com/hillprime/api/internal/auth"
	"github.com/hillprime/api/internal/config"
	"github.com/hillprime/api/internal/models"
	"github.com/hillprime/api/internal/notify"
	"github.com/hillprime/api/internal/store"
	"github.com/rs/zerolog/log"
)

type Auth struct {
	Cfg      *config.Config
	Store    store.Store
	Notifier *notify.Notifier
}

// POST /auth/magic-link {email}
// Always returns 200 — never reveals whether the email exists. This is
// standard practice and protects against user enumeration attacks.
func (h *Auth) MagicLink(w http.ResponseWriter, r *http.Request) {
	var in models.MagicLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if _, err := mail.ParseAddress(in.Email); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid email")
		return
	}

	// Look up; if missing, still return 200.
	user, err := h.Store.GetUserByEmail(r.Context(), in.Email)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Error().Err(err).Msg("auth: lookup")
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if user.Disabled {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	token, err := auth.IssueMagicLink(r.Context(), h.Store, user.ID)
	if err != nil {
		log.Error().Err(err).Msg("auth: issue token")
		writeErr(w, http.StatusInternalServerError, "could not issue token")
		return
	}

	link := h.Cfg.BaseURL + "/auth/callback?token=" + url.QueryEscape(token)
	// Email async so we never block the response on SMTP latency.
	go func(email, link string) {
		if err := h.Notifier.SendMagicLinkEmail(r.Context(), email, link); err != nil {
			log.Error().Err(err).Msg("auth: email magic link")
		}
	}(user.Email, link)

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /auth/callback?token=…
// Consumes the token, mints a session, sets cookie, redirects to the admin app.
func (h *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	cookieValue, expires, err := auth.ExchangeTokenForSession(
		r.Context(), h.Store, token,
		r.Header.Get("User-Agent"), clientIP(r),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "link expired or already used", http.StatusUnauthorized)
			return
		}
		log.Error().Err(err).Msg("auth: exchange token")
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}

	auth.SetSessionCookie(w, cookieValue, expires, h.Cfg.CookieSecure)
	http.Redirect(w, r, h.Cfg.AdminAppURL, http.StatusFound)
}

// POST /auth/logout
func (h *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(auth.CookieName)
	if err == nil && cookie.Value != "" {
		// Best-effort revoke; even if DB is down, we clear the cookie.
		hash := hashForCookie(cookie.Value)
		if sess, err := h.Store.GetSessionByTokenHash(r.Context(), hash); err == nil {
			_ = h.Store.RevokeSession(r.Context(), sess.ID)
		}
	}
	auth.ClearSessionCookie(w, h.Cfg.CookieSecure)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /auth/me
func (h *Auth) Me(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromCtx(r.Context())
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	resp := models.MeResponse{
		ID:    u.ID,
		Email: u.Email,
		Role:  u.Role,
	}
	if u.Name != nil {
		resp.Name = *u.Name
	}
	writeJSON(w, http.StatusOK, resp)
}

// hashForCookie — small duplication so handlers package doesn't import
// a private auth helper.
func hashForCookie(v string) string {
	// Rely on auth package helper via exported token hasher would be nicer,
	// but one-liner duplication is fine.
	return sha256Hex(v)
}
