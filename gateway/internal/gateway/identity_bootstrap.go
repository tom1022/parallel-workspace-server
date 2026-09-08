package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// DefaultAdminUsername is the administrator Identity Bootstrap creates on
// first start, when the User Store has no user yet.
const DefaultAdminUsername = "admin"

// sessionKeySize is the HMAC signing key length for JWTSessionIssuer. 32
// bytes matches SHA-256's block strength, which is what HS256 uses.
const sessionKeySize = 32

// IdentityBootstrap seeds the gateway's own identity state on first start:
// the administrator user, its initial credential, and the key that signs
// sessions. None of this has a source outside this cluster, so — like the
// SSH certificate authority (sshca_bootstrap.go) — the gateway creates it
// here rather than requiring an operator to place it by hand, and whatever
// already exists wins over anything this run would generate (4.3-4.5,
// 4.11).
type IdentityBootstrap struct {
	Users *UserStore

	Dynamic   dynamic.Interface
	Namespace string

	// AdminUsername overrides DefaultAdminUsername.
	AdminUsername string

	// CredentialSecretName holds the plaintext initial credential for an
	// operator to retrieve. It is written once, only at the moment the
	// admin user is first created (4.4) — never touched again, so an
	// operator who deletes it after changing the password does not have it
	// reappear.
	CredentialSecretName  string
	CredentialUsernameKey string // zero means "username"
	CredentialPasswordKey string // zero means "password"

	SessionKeySecretName string
	SessionKeyName       string // zero means "key"
}

// IdentityBootstrapResult is what Ensure produces for the caller to wire into
// the rest of the gateway.
type IdentityBootstrapResult struct {
	// SessionKey signs and verifies the gateway's own sessions (JWTSessionIssuer.Key).
	SessionKey []byte
}

// Ensure creates whatever is missing and returns the current state either
// way, generating nothing when it does not need to.
func (b *IdentityBootstrap) Ensure(ctx context.Context) (*IdentityBootstrapResult, error) {
	if b.Users == nil || b.Dynamic == nil || b.Namespace == "" ||
		b.CredentialSecretName == "" || b.SessionKeySecretName == "" {
		return nil, errors.New("gateway: incomplete identity bootstrap configuration")
	}

	username := b.AdminUsername
	if username == "" {
		username = DefaultAdminUsername
	}

	if err := b.ensureAdmin(ctx, username); err != nil {
		return nil, err
	}

	key, err := b.ensureSessionKey(ctx)
	if err != nil {
		return nil, err
	}

	return &IdentityBootstrapResult{SessionKey: key}, nil
}

// ensureAdmin creates username in the User Store if absent, and publishes
// the password it generated so an operator can retrieve it. When the user
// already existed, nothing was generated and the credential Secret is left
// exactly as it is — present, absent, or already superseded by a password
// change is none of this call's business.
func (b *IdentityBootstrap) ensureAdmin(ctx context.Context, username string) error {
	password, err := randomPassword()
	if err != nil {
		return err
	}
	existed, err := b.Users.EnsureUser(ctx, username, password)
	if err != nil {
		return err
	}
	if existed {
		return nil
	}
	return b.publishInitialCredential(ctx, username, password)
}

func (b *IdentityBootstrap) publishInitialCredential(ctx context.Context, username, password string) error {
	usernameKey := b.CredentialUsernameKey
	if usernameKey == "" {
		usernameKey = "username"
	}
	passwordKey := b.CredentialPasswordKey
	if passwordKey == "" {
		passwordKey = "password"
	}
	secret := newSecretObject(b.Namespace, b.CredentialSecretName, map[string][]byte{
		usernameKey: []byte(username),
		passwordKey: []byte(password),
	})
	_, err := b.Dynamic.Resource(secretGVR).Namespace(b.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err == nil || apierrors.IsAlreadyExists(err) {
		// AlreadyExists here means a concurrent Ensure lost the User Store
		// race (its EnsureUser saw existed=true) but reached this far
		// anyway with its own generated password — this call's password is
		// the one that matches the winning hash, so there is nothing to
		// reconcile against a Secret it did not write.
		return nil
	}
	return fmt.Errorf("gateway: publish initial credential %s/%s: %w", b.Namespace, b.CredentialSecretName, err)
}

func (b *IdentityBootstrap) ensureSessionKey(ctx context.Context) ([]byte, error) {
	keyName := b.SessionKeyName
	if keyName == "" {
		keyName = "key"
	}
	return ensureSecretKey(ctx, b.Dynamic, b.Namespace, b.SessionKeySecretName, keyName, generateSessionKey)
}

func randomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gateway: generate initial credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func generateSessionKey() ([]byte, error) {
	key := make([]byte, sessionKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("gateway: generate session signing key: %w", err)
	}
	return key, nil
}
