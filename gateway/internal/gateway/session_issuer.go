package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultSessionTTL bounds how long a session issued by /auth/login stays
// valid before the caller has to re-authenticate.
const DefaultSessionTTL = time.Hour

// Subject is the caller an Authenticator identified, in a form shared by
// every identification mechanism the gateway holds (external IdP or its own
// User Store).
type Subject struct {
	ID     string
	Name   string
	Source string
}

// SessionIssuer mints and verifies the gateway's own sessions.
type SessionIssuer interface {
	Issue(subject Subject, ttl time.Duration) (string, time.Time, error)
	Verify(token string) (Subject, error)
}

// sessionClaims is the JWT payload behind JWTSessionIssuer. Name and Source
// ride along so Verify can rebuild the full Subject without a second lookup.
//
// IssuedAtNano duplicates RegisteredClaims.IssuedAt at nanosecond resolution.
// The standard "iat" is a whole-second Unix timestamp, which is too coarse
// to compare against UserRecord.UpdatedAt: a password change landing in the
// same second as, but after, issuance would otherwise look like it predates
// the session it is supposed to invalidate.
type sessionClaims struct {
	Name         string `json:"name,omitempty"`
	Source       string `json:"src,omitempty"`
	IssuedAtNano int64  `json:"ian,omitempty"`
	jwt.RegisteredClaims
}

// JWTSessionIssuer implements SessionIssuer as an HMAC-signed JWT. Verify
// additionally rejects a session issued before its subject's User Store
// record was last updated: a credential change (4.6) has to invalidate every
// session minted under the old credential, and UpdatedAt is the only place
// that boundary is recorded, so the check lives here rather than in
// whichever Authenticator ends up calling Verify.
type JWTSessionIssuer struct {
	// Key signs and verifies every session. Issue and Verify both refuse to
	// run without one — minting or trusting a session with no key would be
	// minting or trusting it with an empty key, which is not a signature at
	// all.
	Key []byte
	// Users is consulted on Verify to invalidate sessions that predate a
	// password change. Nil skips that check.
	Users *UserStore
}

// Issue requires a non-empty Subject.ID: a session for nobody in particular
// would verify successfully but identify no one, defeating the point of
// issuing it.
func (s *JWTSessionIssuer) Issue(subject Subject, ttl time.Duration) (string, time.Time, error) {
	if len(s.Key) == 0 {
		return "", time.Time{}, errors.New("gateway: session issuer has no signing key")
	}
	if subject.ID == "" {
		return "", time.Time{}, errors.New("gateway: no subject to issue a session for")
	}
	now := time.Now()
	expiry := now.Add(ttl)
	claims := sessionClaims{
		Name:         subject.Name,
		Source:       subject.Source,
		IssuedAtNano: now.UnixNano(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiry),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.Key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gateway: sign session: %w", err)
	}
	return token, expiry, nil
}

// Verify returns the session's subject only for a token that is unexpired,
// unmodified, and signed by this issuer's own key.
func (s *JWTSessionIssuer) Verify(token string) (Subject, error) {
	if len(s.Key) == 0 {
		return Subject{}, errors.New("gateway: session issuer has no signing key")
	}
	if token == "" {
		return Subject{}, errors.New("gateway: no session token")
	}

	var claims sessionClaims
	parsed, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return s.Key, nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !parsed.Valid {
		return Subject{}, fmt.Errorf("gateway: session rejected: %w", err)
	}
	if claims.Subject == "" {
		return Subject{}, errors.New("gateway: session has no subject")
	}

	if s.Users != nil {
		// ponytail: a live lookup per Verify call, no cache. Add one if the
		// added Kubernetes Get() ever shows up as a bottleneck.
		rec, ok, err := s.Users.Lookup(context.Background(), claims.Subject)
		if err != nil {
			return Subject{}, fmt.Errorf("gateway: session rejected: %w", err)
		}
		issuedAt := time.Unix(0, claims.IssuedAtNano)
		if !ok || rec.UpdatedAt.After(issuedAt) {
			return Subject{}, errors.New("gateway: session predates a credential change")
		}
	}

	return Subject{ID: claims.Subject, Name: claims.Name, Source: claims.Source}, nil
}
