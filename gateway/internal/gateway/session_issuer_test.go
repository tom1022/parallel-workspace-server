package gateway

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newTestSessionIssuer(t *testing.T) (*JWTSessionIssuer, *UserStore) {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schemaGVR]string{secretGVR: "SecretList"},
	)
	users := &UserStore{
		Dynamic:    client,
		Namespace:  "devplatform",
		SecretName: "devplatform-gateway-users",
		BcryptCost: 4,
	}
	return &JWTSessionIssuer{Key: []byte("test-signing-key-0123456789abcd"), Users: users}, users
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	issuer, users := newTestSessionIssuer(t)
	ctx := context.Background()
	if _, err := users.EnsureUser(ctx, "tochi", "s3cret"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	token, expiry, err := issuer.Issue(Subject{ID: "tochi", Name: "tochi", Source: "local"}, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if expiry.Before(time.Now()) {
		t.Fatal("Issue returned an expiry already in the past")
	}

	subject, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if subject.ID != "tochi" || subject.Source != "local" {
		t.Errorf("subject = %+v, want ID=tochi Source=local", subject)
	}
}

func TestIssueRequiresANonEmptySubjectID(t *testing.T) {
	issuer, _ := newTestSessionIssuer(t)
	if _, _, err := issuer.Issue(Subject{}, time.Hour); err == nil {
		t.Fatal("Issue succeeded with an empty Subject.ID")
	}
}

func TestIssueRefusesWithoutASigningKey(t *testing.T) {
	issuer := &JWTSessionIssuer{}
	if _, _, err := issuer.Issue(Subject{ID: "tochi"}, time.Hour); err == nil {
		t.Fatal("Issue succeeded with no signing key configured")
	}
}

func TestVerifyRejectsExpiredTamperedAndForeignlySignedSessions(t *testing.T) {
	issuer, users := newTestSessionIssuer(t)
	ctx := context.Background()
	if _, err := users.EnsureUser(ctx, "tochi", "s3cret"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	expired, _, err := issuer.Issue(Subject{ID: "tochi"}, -time.Minute)
	if err != nil {
		t.Fatalf("Issue expired: %v", err)
	}

	valid, _, err := issuer.Issue(Subject{ID: "tochi"}, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	tampered := valid + "x"

	otherIssuer := &JWTSessionIssuer{Key: []byte("a-completely-different-key-0000"), Users: users}
	foreignlySigned, _, err := otherIssuer.Issue(Subject{ID: "tochi"}, time.Hour)
	if err != nil {
		t.Fatalf("Issue on other issuer: %v", err)
	}

	for name, token := range map[string]string{
		"expired":     expired,
		"tampered":    tampered,
		"foreign key": foreignlySigned,
		"empty":       "",
		"garbage":     "not-a-jwt",
	} {
		if _, err := issuer.Verify(token); err == nil {
			t.Errorf("%s: Verify succeeded, want rejected", name)
		}
	}
}

func TestVerifyRejectsASessionIssuedBeforeAPasswordChange(t *testing.T) {
	issuer, users := newTestSessionIssuer(t)
	ctx := context.Background()
	if _, err := users.EnsureUser(ctx, "tochi", "old-password"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	token, _, err := issuer.Issue(Subject{ID: "tochi", Source: "local"}, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := issuer.Verify(token); err != nil {
		t.Fatalf("Verify before password change: %v, want accepted", err)
	}

	if err := users.SetPassword(ctx, "tochi", "new-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := issuer.Verify(token); err == nil {
		t.Error("Verify after a password change succeeded, want the pre-change session rejected")
	}

	fresh, _, err := issuer.Issue(Subject{ID: "tochi", Source: "local"}, time.Hour)
	if err != nil {
		t.Fatalf("Issue after password change: %v", err)
	}
	if _, err := issuer.Verify(fresh); err != nil {
		t.Errorf("Verify a session issued after the password change: %v, want accepted", err)
	}
}
