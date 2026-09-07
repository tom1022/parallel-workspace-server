package gateway

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://kanidm.example.com/oauth2/openid/devplatform-gateway"
	testAudience = "devplatform-gateway"
)

func newTestVerifier(t *testing.T) (*Verifier, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"kid": "test",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(jwks.Close)
	return &Verifier{Issuer: testIssuer, Audience: testAudience, JWKSURL: jwks.URL}, key
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test"
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "tochi",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
}

func TestVerifyAcceptsWellFormedToken(t *testing.T) {
	v, key := newTestVerifier(t)
	subject, err := v.Verify(signToken(t, key, validClaims()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if subject != "tochi" {
		t.Fatalf("subject = %q, want tochi", subject)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	v, key := newTestVerifier(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	expired := validClaims()
	expired["exp"] = time.Now().Add(-time.Minute).Unix()

	wrongAudience := validClaims()
	wrongAudience["aud"] = "some-other-client"

	wrongIssuer := validClaims()
	wrongIssuer["iss"] = "https://kanidm.example.com/oauth2/openid/argocd"

	// alg=none must not be honoured even though the claims themselves are fine:
	// accepting it would let anyone mint a token without the signing key.
	unsignedTok := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims())
	unsigned, err := unsignedTok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{
		"empty":          "",
		"garbage":        "not-a-jwt",
		"tampered":       signToken(t, key, validClaims()) + "x",
		"foreign key":    signToken(t, other, validClaims()),
		"expired":        signToken(t, key, expired),
		"wrong audience": signToken(t, key, wrongAudience),
		"wrong issuer":   signToken(t, key, wrongIssuer),
		"alg none":       unsigned,
	} {
		if _, err := v.Verify(token); err == nil {
			t.Errorf("%s: Verify succeeded, want error", name)
		}
	}
}

func TestRequireBearerRejectsWithoutRedirect(t *testing.T) {
	v, key := newTestVerifier(t)
	handler := v.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for name, header := range map[string]string{
		"missing":     "",
		"not bearer":  "Basic dXNlcjpwYXNz",
		"tampered":    "Bearer " + signToken(t, key, validClaims()) + "x",
		"no token":    "Bearer ",
		"lowercase":   "bearer " + signToken(t, key, validClaims()),
		"nonsensical": "Bearer ....",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if name == "lowercase" {
			// RFC 7235 makes the scheme case-insensitive; a client that sends
			// it lowercase must still be authenticated, not bounced.
			if rec.Code != http.StatusOK {
				t.Errorf("%s: status = %d, want 200", name, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s: Location = %q, want no login redirect", name, loc)
		}
	}
}

func TestRequireBearerPassesVerifiedRequest(t *testing.T) {
	v, key := newTestVerifier(t)
	var seen string
	handler := v.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = SubjectOf(r)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+signToken(t, key, validClaims()))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "tochi" {
		t.Fatalf("subject in context = %q, want tochi", seen)
	}
}
