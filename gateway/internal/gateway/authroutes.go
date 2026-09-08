package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// LocalAuth wires the gateway's self-hosted authentication endpoints
// (/auth/login, /auth/password) to a User Store and Session Issuer. A nil
// *LocalAuth on Handler disables both endpoints with a 503 without touching
// Identity's authenticator list — the caller keeps the two consistent when
// an operator stops local authentication (4.9).
type LocalAuth struct {
	Users    *UserStore
	Sessions SessionIssuer
	// SessionTTL overrides DefaultSessionTTL for sessions minted here.
	SessionTTL time.Duration
}

func (l *LocalAuth) sessionTTL() time.Duration {
	if l.SessionTTL > 0 {
		return l.SessionTTL
	}
	return DefaultSessionTTL
}

// version answers unauthenticated: a caller has to learn whether it is even
// talking to a compatible gateway before it has anything to authenticate
// with (4.5, 11.4).
func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":          h.Version,
		"minClientVersion": h.MinClientVersion,
		"maxClientVersion": h.MaxClientVersion,
	})
}

// login answers 503 the moment local authentication is not configured,
// before decoding the body at all: an operator who stopped it gets a
// consistent signal regardless of what a caller sends.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if h.Local == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "local authentication is disabled"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password are required"})
		return
	}

	// A wrong password and an unknown username answer identically: neither
	// Verify's error nor this handler's response may tell a caller which one
	// happened, or a login attempt becomes a way to enumerate usernames (4.6).
	if _, err := h.Local.Users.Verify(r.Context(), req.Username, req.Password); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	token, expiry, err := h.Local.Sessions.Issue(Subject{ID: req.Username, Name: req.Username, Source: "local"}, h.Local.sessionTTL())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": token, "expiresAt": expiry})
}

// logout has no server-side state to clear: a session is a self-contained
// signed token, not a reference into a store this gateway keeps, so there is
// nothing to revoke. This only confirms the caller was identified — the
// caller is expected to discard the token itself.
//
// ponytail: no revocation list. Add one (and a real state change here) if a
// use case needs a session invalidated before it expires on its own.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, IdentityOf(r))
}

// changePassword requires the identified caller to prove the current
// credential before accepting a new one — the same check /auth/login makes,
// just against the subject Identity already attached rather than a
// caller-supplied username (4.6).
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	if h.Local == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "local authentication is disabled"})
		return
	}
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CurrentPassword == "" || req.NewPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currentPassword and newPassword are required"})
		return
	}

	username := IdentityOf(r).ID
	if _, err := h.Local.Users.Verify(r.Context(), username, req.CurrentPassword); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	if err := h.Local.Users.SetPassword(r.Context(), username, req.NewPassword); err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// The identified subject is not a User Store entry at all (an
			// IdP-authenticated caller, say) — same signal as a wrong
			// current credential: this account has none to change.
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}
