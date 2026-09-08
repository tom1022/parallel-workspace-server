package gateway

import (
	"context"
	"encoding/base64"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newTestIdentityBootstrap(t *testing.T) *IdentityBootstrap {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schemaGVR]string{secretGVR: "SecretList"},
	)
	return &IdentityBootstrap{
		Users: &UserStore{
			Dynamic:    client,
			Namespace:  "devplatform",
			SecretName: "devplatform-gateway-users",
			BcryptCost: 4,
		},
		Dynamic:              client,
		Namespace:            "devplatform",
		CredentialSecretName: "devplatform-gateway-initial-credential",
		SessionKeySecretName: "devplatform-gateway-session-key",
	}
}

func TestEnsureCreatesAnAdminWithARetrievableInitialCredential(t *testing.T) {
	b := newTestIdentityBootstrap(t)
	ctx := context.Background()

	result, err := b.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(result.SessionKey) == 0 {
		t.Fatal("Ensure produced no session signing key")
	}

	username, password := readCredentialSecret(t, b)
	if username != DefaultAdminUsername {
		t.Errorf("username = %q, want %q", username, DefaultAdminUsername)
	}
	if password == "" {
		t.Fatal("initial credential secret has no password")
	}
	if _, err := b.Users.Verify(ctx, username, password); err != nil {
		t.Errorf("Verify(%q, generated password): %v, want accepted", username, err)
	}
}

func TestEnsureIsIdempotentAcrossRepeatedCalls(t *testing.T) {
	b := newTestIdentityBootstrap(t)
	ctx := context.Background()

	first, err := b.Ensure(ctx)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	_, firstPassword := readCredentialSecret(t, b)

	second, err := b.Ensure(ctx)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	_, secondPassword := readCredentialSecret(t, b)

	if string(first.SessionKey) != string(second.SessionKey) {
		t.Error("session signing key changed across a repeated Ensure; existing sessions would stop verifying")
	}
	if firstPassword != secondPassword {
		t.Error("initial credential changed across a repeated Ensure")
	}
}

func TestEnsureNeverOverwritesAnOperatorChangedPassword(t *testing.T) {
	b := newTestIdentityBootstrap(t)
	ctx := context.Background()
	if _, err := b.Ensure(ctx); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	if err := b.Users.SetPassword(ctx, DefaultAdminUsername, "operator-chosen-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := b.Ensure(ctx); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if _, err := b.Users.Verify(ctx, DefaultAdminUsername, "operator-chosen-password"); err != nil {
		t.Errorf("Verify with operator-chosen password: %v, want accepted", err)
	}
}

func TestEnsureConcurrentlyConvergesOnOneAdminCredential(t *testing.T) {
	b := newTestIdentityBootstrap(t)
	ctx := context.Background()

	type outcome struct {
		result *IdentityBootstrapResult
		err    error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, err := b.Ensure(ctx)
			results <- outcome{r, err}
		}()
	}
	var got [2]outcome
	for i := range got {
		got[i] = <-results
	}
	for i, o := range got {
		if o.err != nil {
			t.Fatalf("Ensure[%d]: %v", i, o.err)
		}
	}
	if string(got[0].result.SessionKey) != string(got[1].result.SessionKey) {
		t.Error("concurrent Ensure calls disagree on the session signing key")
	}

	_, password := readCredentialSecret(t, b)
	if _, err := b.Users.Verify(ctx, DefaultAdminUsername, password); err != nil {
		t.Errorf("Verify with the published initial credential: %v, want accepted", err)
	}
}

func TestEnsureRejectsAnIncompleteIdentityBootstrapConfiguration(t *testing.T) {
	b := newTestIdentityBootstrap(t)
	b.CredentialSecretName = ""
	if _, err := b.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure succeeded without a target credential Secret name")
	}
}

func readCredentialSecret(t *testing.T, b *IdentityBootstrap) (username, password string) {
	t.Helper()
	return readSecretValueGeneric(t, b.Dynamic, b.Namespace, b.CredentialSecretName, "username"),
		readSecretValueGeneric(t, b.Dynamic, b.Namespace, b.CredentialSecretName, "password")
}

func readSecretValueGeneric(t *testing.T, client dynamic.Interface, namespace, name, key string) string {
	t.Helper()
	obj, err := client.Resource(secretGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", namespace, name, err)
	}
	encoded, found, err := unstructured.NestedString(obj.Object, "data", key)
	if err != nil || !found {
		t.Fatalf("secret %s/%s has no key %q", namespace, name, key)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("secret %s/%s key %q is not base64: %v", namespace, name, key, err)
	}
	return string(decoded)
}
