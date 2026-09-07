package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

// argoSyncOptions marks the Secrets this bootstrap writes as off-limits to
// Argo CD. Argo CD renders the chart with `helm template` against no cluster,
// so a chart-rendered Secret could not carry the live key forward and would
// overwrite it on every sync; the key therefore exists only in the cluster,
// and this annotation keeps a later chart change from adopting and pruning it.
const argoSyncOptions = "Prune=false,Delete=false"

// CABootstrap owns the SSH certificate authority's key material. The authority
// has no issuer outside this cluster, so the gateway mints it on first start
// rather than requiring an operator to place one by hand. Whatever key already
// exists wins: regenerating one would invalidate every certificate already
// issued against it, which is also the override path — an operator who wants
// their own authority puts the Secret in place before the gateway starts.
type CABootstrap struct {
	Dynamic dynamic.Interface

	// Namespace/SecretName/KeyName address the private key. It stays in the
	// gateway's own namespace and is never copied elsewhere (14.4/14.5).
	Namespace  string
	SecretName string
	KeyName    string

	// The workspace's sshd trusts this public key and nothing else (4.11), so
	// it is published into the workspace-dedicated namespace.
	PublicKeyNamespace  string
	PublicKeySecretName string
	PublicKeyName       string

	TTL time.Duration
}

// Ensure returns the authority to sign with, creating the key pair and
// publishing the public half when they are not already in the cluster.
func (b *CABootstrap) Ensure(ctx context.Context) (*CertificateAuthority, error) {
	if b.Dynamic == nil || b.Namespace == "" || b.SecretName == "" || b.KeyName == "" ||
		b.PublicKeyNamespace == "" || b.PublicKeySecretName == "" || b.PublicKeyName == "" {
		return nil, errors.New("gateway: incomplete SSH CA bootstrap configuration")
	}

	key, err := b.ensurePrivateKey(ctx)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse SSH CA key from %s/%s: %w", b.Namespace, b.SecretName, err)
	}
	if err := b.publishPublicKey(ctx, signer.PublicKey()); err != nil {
		return nil, err
	}
	return &CertificateAuthority{Signer: signer, TTL: b.TTL}, nil
}

func (b *CABootstrap) ensurePrivateKey(ctx context.Context) ([]byte, error) {
	existing, err := b.readSecretKey(ctx, b.Namespace, b.SecretName, b.KeyName)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return existing, nil
	}

	generated, err := generateCAPrivateKey()
	if err != nil {
		return nil, err
	}
	secret := newSecretObject(b.Namespace, b.SecretName, map[string][]byte{b.KeyName: generated})
	_, err = b.Dynamic.Resource(secretGVR).Namespace(b.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	switch {
	case err == nil:
		return generated, nil
	case !apierrors.IsAlreadyExists(err):
		return nil, fmt.Errorf("gateway: create SSH CA secret %s/%s: %w", b.Namespace, b.SecretName, err)
	}

	// Either another replica created it between the read and the write, or the
	// Secret is there without the expected key. Both are answered by reading
	// back: the first authority to land is the one everyone must sign with.
	adopted, err := b.readSecretKey(ctx, b.Namespace, b.SecretName, b.KeyName)
	if err != nil {
		return nil, err
	}
	if len(adopted) == 0 {
		return nil, fmt.Errorf("gateway: secret %s/%s exists but holds no %q", b.Namespace, b.SecretName, b.KeyName)
	}
	return adopted, nil
}

func (b *CABootstrap) publishPublicKey(ctx context.Context, pub ssh.PublicKey) error {
	authorizedKey := ssh.MarshalAuthorizedKey(pub)
	want := base64.StdEncoding.EncodeToString(authorizedKey)
	client := b.Dynamic.Resource(secretGVR).Namespace(b.PublicKeyNamespace)

	existing, err := client.Get(ctx, b.PublicKeySecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		secret := newSecretObject(b.PublicKeyNamespace, b.PublicKeySecretName, map[string][]byte{b.PublicKeyName: authorizedKey})
		if _, err := client.Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("gateway: publish SSH CA public key to %s/%s: %w", b.PublicKeyNamespace, b.PublicKeySecretName, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("gateway: read secret %s/%s: %w", b.PublicKeyNamespace, b.PublicKeySecretName, err)
	}

	if got, _, _ := unstructured.NestedString(existing.Object, "data", b.PublicKeyName); got == want {
		return nil
	}
	if err := unstructured.SetNestedField(existing.Object, want, "data", b.PublicKeyName); err != nil {
		return err
	}
	if _, err := client.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("gateway: republish SSH CA public key to %s/%s: %w", b.PublicKeyNamespace, b.PublicKeySecretName, err)
	}
	return nil
}

// readSecretKey answers nil for both a missing Secret and a missing key: the
// caller's next step is the same either way.
func (b *CABootstrap) readSecretKey(ctx context.Context, namespace, name, key string) ([]byte, error) {
	obj, err := b.Dynamic.Resource(secretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: read secret %s/%s: %w", namespace, name, err)
	}
	encoded, found, err := unstructured.NestedString(obj.Object, "data", key)
	if err != nil || !found || encoded == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("gateway: secret %s/%s key %q is not base64: %w", namespace, name, key, err)
	}
	return decoded, nil
}

func generateCAPrivateKey() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("gateway: generate SSH CA key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal SSH CA key: %w", err)
	}
	return pem.EncodeToMemory(block), nil
}

func newSecretObject(namespace, name string, data map[string][]byte) *unstructured.Unstructured {
	encoded := map[string]any{}
	for k, v := range data {
		encoded[k] = base64.StdEncoding.EncodeToString(v)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "devplatform-gateway",
			},
			"annotations": map[string]any{
				"argocd.argoproj.io/sync-options": argoSyncOptions,
			},
		},
		"type": "Opaque",
		"data": encoded,
	}}
}
