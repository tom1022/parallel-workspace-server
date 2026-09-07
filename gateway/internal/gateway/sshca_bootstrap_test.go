package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newTestBootstrap(t *testing.T, objs ...runtime.Object) *CABootstrap {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schemaGVR]string{secretGVR: "SecretList"},
		objs...,
	)
	return &CABootstrap{
		Dynamic:             client,
		Namespace:           "devplatform",
		SecretName:          "devplatform-gateway-ssh-ca",
		KeyName:             "id_ca",
		PublicKeyNamespace:  "devplatform-workspaces",
		PublicKeySecretName: "devplatform-workspace-ssh-ca",
		PublicKeyName:       "ca.pub",
		TTL:                 5 * time.Minute,
	}
}

func secretObject(namespace, name string, data map[string]string) *unstructured.Unstructured {
	encoded := map[string]any{}
	for k, v := range data {
		encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"data":       encoded,
	}}
}

func readSecretValue(t *testing.T, b *CABootstrap, namespace, name, key string) string {
	t.Helper()
	obj, err := b.Dynamic.Resource(secretGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
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

func newCAPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

func TestEnsureIssuesACertificateTheWorkspaceCanVerify(t *testing.T) {
	b := newTestBootstrap(t)

	ca, err := b.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	issued, err := ca.Issue("branch-a", newUserKey(t))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(issued.Certificate))
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	cert := parsed.(*ssh.Certificate)

	// The regression this guards: the public key handed to the workspace's
	// sshd has to be the one the gateway signs with, or every certificate is
	// rejected at connect time.
	published := readSecretValue(t, b, "devplatform-workspaces", "devplatform-workspace-ssh-ca", "ca.pub")
	trusted, _, _, _, err := ssh.ParseAuthorizedKey([]byte(published))
	if err != nil {
		t.Fatalf("parse published CA public key: %v", err)
	}
	checker := &ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool {
		return string(k.Marshal()) == string(trusted.Marshal())
	}}
	if err := checker.CheckCert("branch-a", cert); err != nil {
		t.Errorf("CheckCert against published CA: %v", err)
	}
}

func TestEnsureNeverRegeneratesAnExistingKey(t *testing.T) {
	b := newTestBootstrap(t)

	if _, err := b.Ensure(context.Background()); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	first := readSecretValue(t, b, "devplatform", "devplatform-gateway-ssh-ca", "id_ca")
	firstPub := readSecretValue(t, b, "devplatform-workspaces", "devplatform-workspace-ssh-ca", "ca.pub")

	if _, err := b.Ensure(context.Background()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if got := readSecretValue(t, b, "devplatform", "devplatform-gateway-ssh-ca", "id_ca"); got != first {
		t.Error("private key was regenerated; already-issued certificates would stop verifying")
	}
	if got := readSecretValue(t, b, "devplatform-workspaces", "devplatform-workspace-ssh-ca", "ca.pub"); got != firstPub {
		t.Error("published public key changed without the private key changing")
	}
}

func TestEnsureAdoptsAnOperatorSuppliedKey(t *testing.T) {
	supplied := newCAPrivateKeyPEM(t)
	b := newTestBootstrap(t, secretObject("devplatform", "devplatform-gateway-ssh-ca", map[string]string{"id_ca": supplied}))

	ca, err := b.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if got := readSecretValue(t, b, "devplatform", "devplatform-gateway-ssh-ca", "id_ca"); got != supplied {
		t.Fatal("Ensure overwrote a pre-existing CA key")
	}
	published := readSecretValue(t, b, "devplatform-workspaces", "devplatform-workspace-ssh-ca", "ca.pub")
	if published != string(ssh.MarshalAuthorizedKey(ca.Signer.PublicKey())) {
		t.Error("published public key does not belong to the adopted private key")
	}
}

func TestEnsureRepublishesAStalePublicKey(t *testing.T) {
	b := newTestBootstrap(t,
		secretObject("devplatform", "devplatform-gateway-ssh-ca", map[string]string{"id_ca": newCAPrivateKeyPEM(t)}),
		secretObject("devplatform-workspaces", "devplatform-workspace-ssh-ca", map[string]string{"ca.pub": "ssh-ed25519 AAAAstale\n"}),
	)

	ca, err := b.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	published := readSecretValue(t, b, "devplatform-workspaces", "devplatform-workspace-ssh-ca", "ca.pub")
	if published != string(ssh.MarshalAuthorizedKey(ca.Signer.PublicKey())) {
		t.Error("stale public key was left in place; the workspace would trust the wrong authority")
	}
}

func TestEnsureRejectsAnIncompleteConfiguration(t *testing.T) {
	b := newTestBootstrap(t)
	b.SecretName = ""
	if _, err := b.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure succeeded without a target Secret name")
	}
}
