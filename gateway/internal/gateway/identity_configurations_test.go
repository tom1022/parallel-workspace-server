package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// identityFixture builds real (not stubbed) IdentityService components so
// the four configurations below exercise the actual IDPAuthenticator and
// LocalAuthenticator implementations, not test doubles.
type identityFixture struct {
	idpToken   string
	localToken string
	idp        Authenticator
	local      Authenticator
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	verifier, key := newTestVerifier(t)
	issuer, users := newTestSessionIssuer(t)
	ctx := context.Background()
	if _, err := users.EnsureUser(ctx, "tochi", "s3cret"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	localToken, _, err := issuer.Issue(Subject{ID: "tochi", Name: "tochi", Source: "local"}, DefaultSessionTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return &identityFixture{
		idpToken:   signToken(t, key, validClaims()),
		localToken: localToken,
		idp:        &IDPAuthenticator{Verifier: verifier},
		local:      &LocalAuthenticator{Issuer: issuer},
	}
}

func identifyWithToken(t *testing.T, id *Identity, token string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	_, err := id.Identify(context.Background(), req)
	return err
}

// 4.4's four configurations: local only, IdP only, both together, and local
// stopped. Each must accept exactly the tokens its enabled mechanisms mint
// and reject everything else, including the disabled mechanism's own token.
func TestFourIdentityConfigurationsAcceptAndRejectAsExpected(t *testing.T) {
	f := newIdentityFixture(t)

	t.Run("local only", func(t *testing.T) {
		id := &Identity{Authenticators: []Authenticator{f.local}}
		if err := identifyWithToken(t, id, f.localToken); err != nil {
			t.Errorf("local token rejected: %v", err)
		}
		if err := identifyWithToken(t, id, f.idpToken); err == nil {
			t.Error("idp token accepted, want rejected (idp not configured)")
		}
	})

	t.Run("idp only", func(t *testing.T) {
		id := &Identity{Authenticators: []Authenticator{f.idp}}
		if err := identifyWithToken(t, id, f.idpToken); err != nil {
			t.Errorf("idp token rejected: %v", err)
		}
		if err := identifyWithToken(t, id, f.localToken); err == nil {
			t.Error("local token accepted, want rejected (local not configured)")
		}
	})

	t.Run("both combined", func(t *testing.T) {
		id := &Identity{Authenticators: []Authenticator{f.idp, f.local}}
		if err := identifyWithToken(t, id, f.idpToken); err != nil {
			t.Errorf("idp token rejected: %v", err)
		}
		if err := identifyWithToken(t, id, f.localToken); err != nil {
			t.Errorf("local token rejected: %v", err)
		}
	})

	t.Run("local stopped", func(t *testing.T) {
		// Same shape as "idp only": an operator who stops local
		// authentication simply never wires LocalAuthenticator in.
		id := &Identity{Authenticators: []Authenticator{f.idp}}
		if err := identifyWithToken(t, id, f.localToken); err == nil {
			t.Error("local token accepted after local authentication was stopped")
		}
		if err := identifyWithToken(t, id, f.idpToken); err != nil {
			t.Errorf("idp token rejected after stopping local authentication: %v", err)
		}
	})

	t.Run("neither configured refuses everything", func(t *testing.T) {
		id := &Identity{}
		if err := identifyWithToken(t, id, f.idpToken); err == nil {
			t.Error("Identify succeeded with no authenticators configured")
		}
	})
}
