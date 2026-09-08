package gateway

import (
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authTestRig wires the pieces /auth/ and /version need: a Handler with an
// external IdP Authenticator always present (so a caller can be identified
// even while local authentication is disabled) and local authentication
// configured or not, per the four-configuration matrix task 4.4 covers
// separately.
type authTestRig struct {
	handler http.Handler
	users   *UserStore
	issuer  *JWTSessionIssuer
	idpKey  *rsa.PrivateKey
}

func newAuthTestRig(t *testing.T, localEnabled bool) *authTestRig {
	t.Helper()
	issuer, users := newTestSessionIssuer(t)
	verifier, idpKey := newTestVerifier(t)

	authenticators := []Authenticator{&IDPAuthenticator{Verifier: verifier}}
	h := &Handler{Store: &Store{}}
	if localEnabled {
		authenticators = append(authenticators, &LocalAuthenticator{Issuer: issuer})
		h.Local = &LocalAuth{Users: users, Sessions: issuer}
	}
	h.Identity = &Identity{Authenticators: authenticators}

	return &authTestRig{handler: h.Routes(), users: users, issuer: issuer, idpKey: idpKey}
}

// withIDPBearer identifies the caller through the external IdP mechanism
// rather than a local session — used to prove that disabling local
// authentication refuses /auth/login and /auth/password even for a caller
// some other mechanism has identified.
func (r *authTestRig) idpToken(t *testing.T) string {
	t.Helper()
	return signToken(t, r.idpKey, validClaims())
}

func (r *authTestRig) do(t *testing.T, method, target, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

func withBearer(token string) func(*http.Request) {
	return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+token) }
}

func TestVersionRequiresNoIdentification(t *testing.T) {
	rig := newAuthTestRig(t, true)
	rec := rig.do(t, http.MethodGet, "/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestLoginWithInitialCredentialThenRejectsItAfterAChange(t *testing.T) {
	rig := newAuthTestRig(t, true)
	if _, err := rig.users.EnsureUser(t.Context(), "admin", "initial-password"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	rec := rig.do(t, http.MethodPost, "/auth/login", `{"username":"admin","password":"initial-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Session   string `json:"session"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if body.Session == "" || body.ExpiresAt == "" {
		t.Fatalf("login response missing session or expiresAt: %s", rec.Body.String())
	}

	// The returned session actually identifies the caller.
	me := rig.do(t, http.MethodGet, "/auth/me", "", withBearer(body.Session))
	if me.Code != http.StatusOK {
		t.Fatalf("GET /auth/me status = %d, want 200", me.Code)
	}

	if err := rig.users.SetPassword(t.Context(), "admin", "new-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	rec = rig.do(t, http.MethodPost, "/auth/login", `{"username":"admin","password":"initial-password"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password status = %d, want 401", rec.Code)
	}
	rec = rig.do(t, http.MethodPost, "/auth/login", `{"username":"admin","password":"new-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with new password status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginAnswersIdenticallyForUnknownUserAndWrongPassword(t *testing.T) {
	rig := newAuthTestRig(t, true)
	if _, err := rig.users.EnsureUser(t.Context(), "admin", "correct-password"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	wrongPassword := rig.do(t, http.MethodPost, "/auth/login", `{"username":"admin","password":"wrong"}`)
	unknownUser := rig.do(t, http.MethodPost, "/auth/login", `{"username":"nobody","password":"wrong"}`)

	if wrongPassword.Code != http.StatusUnauthorized || unknownUser.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d / %d, want both 401", wrongPassword.Code, unknownUser.Code)
	}
	if wrongPassword.Body.String() != unknownUser.Body.String() {
		t.Errorf("responses differ: %q vs %q — this leaks whether a username exists", wrongPassword.Body.String(), unknownUser.Body.String())
	}
}

func TestLoginAndPasswordChangeAreDisabledWithoutLocalAuth(t *testing.T) {
	rig := newAuthTestRig(t, false)
	if rec := rig.do(t, http.MethodPost, "/auth/login", `{"username":"admin","password":"x"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("login status = %d, want 503", rec.Code)
	}
	// Identified through the still-active IdP mechanism: local being
	// disabled refuses the action itself, not just unauthenticated callers.
	rec := rig.do(t, http.MethodPost, "/auth/password", `{"currentPassword":"x","newPassword":"y"}`, withBearer(rig.idpToken(t)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("password status = %d, want 503", rec.Code)
	}
}

func TestChangePasswordRequiresTheCurrentCredential(t *testing.T) {
	rig := newAuthTestRig(t, true)
	if _, err := rig.users.EnsureUser(t.Context(), "tochi", "old-password"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	token, _, err := rig.issuer.Issue(Subject{ID: "tochi", Source: "local"}, DefaultSessionTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	bad := rig.do(t, http.MethodPost, "/auth/password", `{"currentPassword":"wrong","newPassword":"new-password"}`, withBearer(token))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password status = %d, want 401", bad.Code)
	}

	malformed := rig.do(t, http.MethodPost, "/auth/password", `{"currentPassword":"old-password"}`, withBearer(token))
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("missing newPassword status = %d, want 400", malformed.Code)
	}

	ok := rig.do(t, http.MethodPost, "/auth/password", `{"currentPassword":"old-password","newPassword":"new-password"}`, withBearer(token))
	if ok.Code != http.StatusOK {
		t.Fatalf("password change status = %d, want 200: %s", ok.Code, ok.Body.String())
	}

	if _, err := rig.users.Verify(t.Context(), "tochi", "old-password"); err == nil {
		t.Error("old password still verifies after a change")
	}
	if _, err := rig.users.Verify(t.Context(), "tochi", "new-password"); err != nil {
		t.Errorf("new password does not verify: %v", err)
	}
}

func TestChangePasswordAndMeAndLogoutRequireASession(t *testing.T) {
	rig := newAuthTestRig(t, true)
	if rec := rig.do(t, http.MethodPost, "/auth/password", `{"currentPassword":"x","newPassword":"y"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("password status = %d, want 401", rec.Code)
	}
	if rec := rig.do(t, http.MethodGet, "/auth/me", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("me status = %d, want 401", rec.Code)
	}
	if rec := rig.do(t, http.MethodPost, "/auth/logout", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("logout status = %d, want 401", rec.Code)
	}
}

func TestMeReturnsTheIdentifiedSubject(t *testing.T) {
	rig := newAuthTestRig(t, true)
	if _, err := rig.users.EnsureUser(t.Context(), "tochi", "s3cret"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	token, _, err := rig.issuer.Issue(Subject{ID: "tochi", Name: "tochi", Source: "local"}, DefaultSessionTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	rec := rig.do(t, http.MethodGet, "/auth/me", "", withBearer(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var subject Subject
	if err := json.Unmarshal(rec.Body.Bytes(), &subject); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if subject.ID != "tochi" || subject.Source != "local" {
		t.Errorf("subject = %+v, want ID=tochi Source=local", subject)
	}
}

func TestLogoutSucceedsForAnIdentifiedCaller(t *testing.T) {
	rig := newAuthTestRig(t, true)
	if _, err := rig.users.EnsureUser(t.Context(), "tochi", "s3cret"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	token, _, err := rig.issuer.Issue(Subject{ID: "tochi", Source: "local"}, DefaultSessionTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	rec := rig.do(t, http.MethodPost, "/auth/logout", "", withBearer(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
