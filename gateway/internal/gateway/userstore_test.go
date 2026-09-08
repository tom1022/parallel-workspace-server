package gateway

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newTestUserStore(t *testing.T) *UserStore {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schemaGVR]string{secretGVR: "SecretList"},
	)
	return &UserStore{
		Dynamic:    client,
		Namespace:  "devplatform",
		SecretName: "devplatform-gateway-users",
		// A low cost keeps the test suite fast; production wiring uses the
		// bcrypt default.
		BcryptCost: 4,
	}
}

func TestEnsureUserCreatesOnFirstCallOnly(t *testing.T) {
	s := newTestUserStore(t)
	ctx := context.Background()

	existed, err := s.EnsureUser(ctx, "admin", "first-password")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if existed {
		t.Fatal("EnsureUser reported the admin user already existed on first call")
	}

	existed, err = s.EnsureUser(ctx, "admin", "second-password")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if !existed {
		t.Fatal("EnsureUser reported the admin user as new on second call")
	}

	// The regression this guards: a second EnsureUser call must never
	// overwrite the credential the first call set.
	if _, err := s.Verify(ctx, "admin", "first-password"); err != nil {
		t.Errorf("Verify(first-password): %v, want accepted", err)
	}
	if _, err := s.Verify(ctx, "admin", "second-password"); err == nil {
		t.Error("Verify(second-password) succeeded, want rejected")
	}
}

func TestVerifyAcceptsCorrectAndRejectsWrongCredential(t *testing.T) {
	s := newTestUserStore(t)
	ctx := context.Background()
	if _, err := s.EnsureUser(ctx, "tochi", "correct-horse-battery-staple"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	if _, err := s.Verify(ctx, "tochi", "correct-horse-battery-staple"); err != nil {
		t.Errorf("Verify with correct credential: %v, want accepted", err)
	}
	if _, err := s.Verify(ctx, "tochi", "wrong"); err == nil {
		t.Error("Verify with wrong credential succeeded, want rejected")
	}
	if _, err := s.Verify(ctx, "no-such-user", "anything"); err == nil {
		t.Error("Verify for unknown user succeeded, want rejected")
	}
}

func TestSetPasswordRejectsThePreviousCredential(t *testing.T) {
	s := newTestUserStore(t)
	ctx := context.Background()
	if _, err := s.EnsureUser(ctx, "tochi", "old-password"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	if err := s.SetPassword(ctx, "tochi", "new-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := s.Verify(ctx, "tochi", "old-password"); err == nil {
		t.Error("Verify with the previous credential succeeded, want rejected")
	}
	if _, err := s.Verify(ctx, "tochi", "new-password"); err != nil {
		t.Errorf("Verify with the new credential: %v, want accepted", err)
	}
}

func TestUserStoreSurvivesAFreshInstanceOverTheSameSecret(t *testing.T) {
	// Simulates a gateway restart: a new *UserStore backed by the same
	// Secret must see what a previous instance wrote.
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schemaGVR]string{secretGVR: "SecretList"},
	)
	first := &UserStore{Dynamic: client, Namespace: "devplatform", SecretName: "devplatform-gateway-users", BcryptCost: 4}
	ctx := context.Background()
	if _, err := first.EnsureUser(ctx, "admin", "s3cret"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	second := &UserStore{Dynamic: client, Namespace: "devplatform", SecretName: "devplatform-gateway-users", BcryptCost: 4}
	if _, err := second.Verify(ctx, "admin", "s3cret"); err != nil {
		t.Errorf("Verify on a fresh instance: %v, want accepted", err)
	}
}

func TestUserStoreDoesNotPersistThePasswordInPlainText(t *testing.T) {
	s := newTestUserStore(t)
	ctx := context.Background()
	if _, err := s.EnsureUser(ctx, "admin", "do-not-leak-me"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	rec, ok, err := s.Lookup(ctx, "admin")
	if err != nil || !ok {
		t.Fatalf("Lookup: ok=%v err=%v", ok, err)
	}
	if rec.PasswordHash == "do-not-leak-me" {
		t.Error("stored credential material is the plaintext password")
	}
	if rec.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not recorded")
	}
}
