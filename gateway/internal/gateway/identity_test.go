package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubAuthenticator struct {
	name    string
	accept  string // token this authenticator accepts; "" accepts none
	subject Subject
}

func (a *stubAuthenticator) Name() string { return a.name }

func (a *stubAuthenticator) Identify(_ context.Context, r *http.Request) (Subject, error) {
	if a.accept == "" || PresentedToken(r) != a.accept {
		return Subject{}, errors.New(a.name + ": rejected")
	}
	return a.subject, nil
}

func TestIdentityRefusesToIdentifyWithNoAuthenticatorsConfigured(t *testing.T) {
	id := &Identity{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := id.Identify(context.Background(), req); err == nil {
		t.Fatal("Identify succeeded with zero authenticators configured")
	}
}

func TestIdentityAcceptsWhenExactlyOneAuthenticatorAccepts(t *testing.T) {
	id := &Identity{Authenticators: []Authenticator{
		&stubAuthenticator{name: "idp", accept: "idp-token", subject: Subject{ID: "u1", Source: "idp"}},
		&stubAuthenticator{name: "local", accept: "local-token", subject: Subject{ID: "u2", Source: "local"}},
	}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer local-token")
	subject, err := id.Identify(context.Background(), req)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if subject.ID != "u2" || subject.Source != "local" {
		t.Errorf("subject = %+v, want the local authenticator's subject", subject)
	}
}

func TestIdentityRejectsWhenNoAuthenticatorAccepts(t *testing.T) {
	id := &Identity{Authenticators: []Authenticator{
		&stubAuthenticator{name: "idp", accept: "idp-token"},
		&stubAuthenticator{name: "local", accept: "local-token"},
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer something-else")
	if _, err := id.Identify(context.Background(), req); err == nil {
		t.Fatal("Identify succeeded although no authenticator accepted the request")
	}
}

func TestRequireAttachesSubjectAndRejectsWithoutLeakingWhy(t *testing.T) {
	id := &Identity{Authenticators: []Authenticator{
		&stubAuthenticator{name: "local", accept: "good-token", subject: Subject{ID: "u1", Name: "tochi", Source: "local"}},
	}}
	var seen Subject
	handler := id.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = IdentityOf(r)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen.ID != "u1" || seen.Name != "tochi" {
		t.Errorf("subject in context = %+v, want ID=u1 Name=tochi", seen)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec2.Code)
	}
	if loc := rec2.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want no redirect", loc)
	}
}

func TestPresentedTokenReadsHeaderThenWebSocketSubprotocol(t *testing.T) {
	header := httptest.NewRequest(http.MethodGet, "/ws", nil)
	header.Header.Set("Authorization", "Bearer from-header")
	if got := PresentedToken(header); got != "from-header" {
		t.Errorf("PresentedToken(header) = %q, want from-header", got)
	}

	ws := httptest.NewRequest(http.MethodGet, "/ws", nil)
	ws.Header.Set("Sec-WebSocket-Protocol", "bearer.from-subprotocol")
	if got := PresentedToken(ws); got != "from-subprotocol" {
		t.Errorf("PresentedToken(ws) = %q, want from-subprotocol", got)
	}

	none := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if got := PresentedToken(none); got != "" {
		t.Errorf("PresentedToken(none) = %q, want empty", got)
	}
}

func TestIDPAuthenticatorWrapsTheExistingVerifierUnchanged(t *testing.T) {
	v, key := newTestVerifier(t)
	a := &IDPAuthenticator{Verifier: v}

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+signToken(t, key, validClaims()))
	subject, err := a.Identify(context.Background(), req)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if subject.ID != "tochi" || subject.Source != a.Name() {
		t.Errorf("subject = %+v, want ID=tochi Source=%s", subject, a.Name())
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	if _, err := a.Identify(context.Background(), bad); err == nil {
		t.Error("Identify succeeded without a bearer token")
	}
}

func TestLocalAuthenticatorWrapsASessionIssuer(t *testing.T) {
	issuer, users := newTestSessionIssuer(t)
	ctx := context.Background()
	if _, err := users.EnsureUser(ctx, "tochi", "s3cret"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	token, _, err := issuer.Issue(Subject{ID: "tochi", Source: "local"}, DefaultSessionTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	a := &LocalAuthenticator{Issuer: issuer}
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	subject, err := a.Identify(ctx, req)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if subject.ID != "tochi" {
		t.Errorf("subject.ID = %q, want tochi", subject.ID)
	}
}
