package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Authenticator identifies the caller behind one request. The gateway holds
// one or more; a request is accepted when exactly one accepts it.
type Authenticator interface {
	// Name identifies the mechanism in logs and in Subject.Source.
	Name() string
	// Identify returns the authenticated subject, or an error when this
	// mechanism does not accept the request.
	Identify(ctx context.Context, r *http.Request) (Subject, error)
}

// ErrNoAuthenticators marks an Identity with no configured Authenticator. It
// must refuse to identify anyone rather than accept every request by
// default: "識別できない要求を拒む" (4.1) is not a state a missing
// configuration gets to opt out of.
var ErrNoAuthenticators = errors.New("gateway: no authenticator configured")

// ErrUnidentified is returned when no configured Authenticator accepted a
// request.
var ErrUnidentified = errors.New("gateway: request could not be identified")

// Identity identifies the caller behind a request by trying each configured
// Authenticator in turn and accepting the first that succeeds.
type Identity struct {
	Authenticators []Authenticator
}

// Identify returns the first accepting Authenticator's Subject. A nil
// Identity behaves like one configured with zero Authenticators: it refuses
// every request rather than panicking on the unconfigured case (4.1's
// invariant applies to a Handler nobody wired an Identity into as much as to
// one wired with an empty list).
func (id *Identity) Identify(ctx context.Context, r *http.Request) (Subject, error) {
	if id == nil || len(id.Authenticators) == 0 {
		return Subject{}, ErrNoAuthenticators
	}
	for _, a := range id.Authenticators {
		if subject, err := a.Identify(ctx, r); err == nil && subject.ID != "" {
			return subject, nil
		}
	}
	return Subject{}, ErrUnidentified
}

type identityKey struct{}

// Require gates next behind Identify, attaching the resulting Subject to the
// request context. A rejected request gets a plain 401 and nothing else:
// telling an unidentified caller why would tell it more about the
// deployment than it is owed (4.1).
func (id *Identity) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, err := id.Identify(r.Context(), r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="devplatform-gateway"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, subject)))
	})
}

// IdentityOf returns the Subject a request's Identity.Require attached, or
// the zero Subject for any other request.
func IdentityOf(r *http.Request) Subject {
	subject, _ := r.Context().Value(identityKey{}).(Subject)
	return subject
}

// wsProtocolBearerPrefix is the Sec-WebSocket-Protocol convention a browser
// uses to present a credential where it cannot set a header: a single
// dot-joined token with no comma, which is what keeps the client's offered
// protocol list at length 1 so the handshake's default protocol echo
// succeeds without a custom negotiation step.
const wsProtocolBearerPrefix = "bearer."

// PresentedToken extracts a bearer-shaped credential from a request. Every
// Authenticator reads through this so a caller has exactly one way to
// present a given credential regardless of which mechanism ends up
// accepting it: the Authorization header for a plain HTTP request, and the
// Sec-WebSocket-Protocol negotiation where a WebSocket handshake cannot
// carry a custom header.
func PresentedToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	for _, proto := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		proto = strings.TrimSpace(proto)
		if len(proto) > len(wsProtocolBearerPrefix) && strings.EqualFold(proto[:len(wsProtocolBearerPrefix)], wsProtocolBearerPrefix) {
			return proto[len(wsProtocolBearerPrefix):]
		}
	}
	return ""
}

// IDPAuthenticator adapts the existing external-IdP Bearer verification to
// the Authenticator abstraction. Behavior is unchanged from RequireBearer
// (4.8); only the credential lookup now goes through PresentedToken so a
// caller can present the same token over the WebSocket sub-protocol too.
type IDPAuthenticator struct {
	Verifier *Verifier
}

func (a *IDPAuthenticator) Name() string { return "idp" }

func (a *IDPAuthenticator) Identify(_ context.Context, r *http.Request) (Subject, error) {
	subject, err := a.Verifier.Verify(PresentedToken(r))
	if err != nil {
		return Subject{}, err
	}
	return Subject{ID: subject, Name: subject, Source: a.Name()}, nil
}

// LocalAuthenticator adapts the gateway's own Session Issuer to the
// Authenticator abstraction, so a session minted by /auth/login is accepted
// through the same request path as an external IdP token (4.2, 4.9).
type LocalAuthenticator struct {
	Issuer SessionIssuer
}

func (a *LocalAuthenticator) Name() string { return "local" }

func (a *LocalAuthenticator) Identify(_ context.Context, r *http.Request) (Subject, error) {
	return a.Issuer.Verify(PresentedToken(r))
}
