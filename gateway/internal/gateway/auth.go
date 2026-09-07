package gateway

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type subjectKey struct{}

// Verifier checks the Bearer tokens presented to /api/. The gateway does this
// itself rather than trusting the forward auth chain in front of it: that chain
// is configured to let Bearer-carrying requests through untouched for this
// hostname, so passing it proves nothing about authorization (4.12).
type Verifier struct {
	Issuer   string
	Audience string
	JWKSURL  string

	// HTTPClient is used to fetch the JWKS. Nil means http.DefaultClient.
	HTTPClient *http.Client
	// RefreshInterval bounds how stale a cached JWKS may be. Zero means 1 hour.
	RefreshInterval time.Duration

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// Verify returns the token's subject, or an error if the token is missing,
// malformed, unsigned, signed by an unknown key, expired, or issued for a
// different issuer or audience.
func (v *Verifier) Verify(token string) (string, error) {
	if token == "" {
		return "", errors.New("gateway: no bearer token")
	}
	parsed, err := jwt.Parse(token, v.keyFunc,
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}),
		jwt.WithIssuer(v.Issuer),
		jwt.WithAudience(v.Audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return "", fmt.Errorf("gateway: bearer token rejected: %w", err)
	}
	subject, err := parsed.Claims.GetSubject()
	if err != nil || subject == "" {
		return "", errors.New("gateway: bearer token has no subject")
	}
	return subject, nil
}

// RequireBearer gates next behind Verify. A rejected request gets a plain 401
// and never a redirect: the caller is a program holding a token, and answering
// with a login page would turn an authorization failure into an HTML body that
// a client cannot act on (4.12).
func (v *Verifier) RequireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, err := v.Verify(bearerToken(r))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="devplatform-gateway"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), subjectKey{}, subject)))
	})
}

// SubjectOf returns the verified token subject carried by a request that passed
// RequireBearer, or "" for any other request.
func SubjectOf(r *http.Request) string {
	subject, _ := r.Context().Value(subjectKey{}).(string)
	return subject
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func (v *Verifier) keyFunc(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	key, err := v.key(kid)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func (v *Verifier) key(kid string) (*rsa.PublicKey, error) {
	if key := v.cachedKey(kid); key != nil {
		return key, nil
	}
	if err := v.refresh(); err != nil {
		return nil, err
	}
	if key := v.cachedKey(kid); key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("gateway: no signing key %q in JWKS", kid)
}

func (v *Verifier) cachedKey(kid string) *rsa.PublicKey {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if time.Since(v.fetchedAt) > v.refreshInterval() {
		return nil
	}
	return v.keys[kid]
}

func (v *Verifier) refreshInterval() time.Duration {
	if v.RefreshInterval > 0 {
		return v.RefreshInterval
	}
	return time.Hour
}

func (v *Verifier) refresh() error {
	client := v.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Get(v.JWKSURL)
	if err != nil {
		return fmt.Errorf("gateway: fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway: fetch JWKS: status %d", resp.StatusCode)
	}

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("gateway: decode JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}

	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
