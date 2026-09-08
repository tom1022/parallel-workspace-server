package gateway

import (
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

func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "local authentication is disabled"})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, IdentityOf(r))
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "local authentication is disabled"})
}
