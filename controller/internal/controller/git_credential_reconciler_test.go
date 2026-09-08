package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// fakeGitCredentialIssuer is the test double for a real Git hosting API,
// which no implementation in this package vendors (see
// git_credential_reconciler.go's package comment). It records every request
// so tests can assert on the scope the reconciler asked to be enforced.
type fakeGitCredentialIssuer struct {
	requests []GitCredentialRequest
	err      error
}

func (f *fakeGitCredentialIssuer) Issue(_ context.Context, req GitCredentialRequest) (IssuedGitCredential, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return IssuedGitCredential{}, f.err
	}
	return IssuedGitCredential{
		SecretData: map[string][]byte{"token": []byte("fake-token-" + req.WorkspaceId)},
		Handle:     "fake-handle-" + req.WorkspaceId,
	}, nil
}

func TestNormalizeRepositoryIdentity(t *testing.T) {
	cases := map[string]string{
		"acme/widgets":                             "acme/widgets",
		"git@git.example.com:acme/widgets.git":     "acme/widgets",
		"https://git.example.com/acme/widgets.git": "acme/widgets",
		"https://git.example.com/acme/widgets":     "acme/widgets",
		"Acme/Widgets":                             "acme/widgets",
		"https://git.example.com/other/demo.git":   "other/demo",
	}
	for in, want := range cases {
		if got := normalizeRepositoryIdentity(in); got != want {
			t.Errorf("normalizeRepositoryIdentity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReconcileGitCredential_IssuesRepositoryAndBranchScopedSecret(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	repo := "https://git.example.com/acme/demo.git"
	branch := "feature/git-cred"
	ws := createWorkspace(t, ctx, ns, "ws-gitcred", repo, branch, "default")

	fake := &fakeGitCredentialIssuer{}
	r := newTestReconciler()
	r.GitCredentialIssuer = fake

	resourceName := "feature-git-cred"
	if err := r.reconcileGitCredential(ctx, ws, resourceName); err != nil {
		t.Fatalf("reconcileGitCredential: %v", err)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("issuer called %d times, want 1", len(fake.requests))
	}
	req := fake.requests[0]
	if req.Repository != repo {
		t.Errorf("request.Repository = %q, want %q (5.1: scoped to the target repository only)", req.Repository, repo)
	}
	if req.AllowedBranch != branch {
		t.Errorf("request.AllowedBranch = %q, want %q (push limited to the working branch)", req.AllowedBranch, branch)
	}
	if !req.RejectDefaultBranchPush {
		t.Error("request.RejectDefaultBranchPush = false, want true")
	}

	var secret corev1.Secret
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &secret); err != nil {
		t.Fatalf("get git credential Secret: %v", err)
	}
	if string(secret.Data["token"]) != "fake-token-"+resourceName {
		t.Errorf("Secret data = %q, want the issuer's returned credential material", secret.Data)
	}
	if secret.Annotations[gitCredentialHandleAnnotation] != "fake-handle-"+resourceName {
		t.Errorf("Secret annotation %s = %q, want the issuer's handle preserved for future revocation", gitCredentialHandleAnnotation, secret.Annotations[gitCredentialHandleAnnotation])
	}
	if len(secret.GetOwnerReferences()) != 1 || secret.GetOwnerReferences()[0].Name != ws.Name {
		t.Errorf("Secret ownerReferences = %+v, want a single reference to %s", secret.GetOwnerReferences(), ws.Name)
	}

	// Idempotency: reconciling again must not re-issue (a real issuer call has
	// external side effects, unlike the k8s-only reconcilers).
	if err := r.reconcileGitCredential(ctx, ws, resourceName); err != nil {
		t.Fatalf("second reconcileGitCredential: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("issuer called %d times after a second reconcile, want still 1 (idempotent)", len(fake.requests))
	}
}

func TestReconcileGitCredential_GuardRefusesDeclaredProtectedRepository(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	protectedSpellings := []string{
		"acme/gitops-config",
		"git@git.example.com:acme/gitops-config.git",
		"https://git.example.com/acme/gitops-config.git",
	}
	for i, repo := range protectedSpellings {
		ws := createWorkspace(t, ctx, ns, "ws-protected-repo"+string(rune('a'+i)), repo, "feature/x", "default")

		fake := &fakeGitCredentialIssuer{}
		r := newTestReconciler()
		r.GitCredentialIssuer = fake
		r.GitCredentialGuard = GitCredentialGuard{ProtectedRepositories: []string{"acme/gitops-config"}}

		err := r.reconcileGitCredential(ctx, ws, "resource-"+string(rune('a'+i)))
		if err == nil {
			t.Fatalf("reconcileGitCredential(%q) = nil error, want a refusal (5.5: declared protected repository)", repo)
		}
		if len(fake.requests) != 0 {
			t.Errorf("issuer was called for protected repository %q; the guard must run first", repo)
		}
	}
}

func TestReconcileGitCredential_GuardRefusesDeclaredProtectedBranch(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := createWorkspace(t, ctx, ns, "ws-protected-branch", "https://git.example.com/acme/demo.git", "main", "default")

	fake := &fakeGitCredentialIssuer{}
	r := newTestReconciler()
	r.GitCredentialIssuer = fake
	r.GitCredentialGuard = GitCredentialGuard{ProtectedBranches: []string{"main"}}

	err := r.reconcileGitCredential(ctx, ws, "resource-protected-branch")
	if err == nil {
		t.Fatal("reconcileGitCredential = nil error, want a refusal (5.6: declared protected branch)")
	}
	if len(fake.requests) != 0 {
		t.Error("issuer was called for a protected branch; the guard must run first")
	}
}

func TestReconcileGitCredential_EmptyGuardRefusesNothing(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	// Same repository/branch name a provider-specific deployment might have
	// wanted protected, but this deployment declared no Guard at all.
	ws := createWorkspace(t, ctx, ns, "ws-no-guard", "https://git.example.com/acme/gitops-config.git", "main", "default")

	fake := &fakeGitCredentialIssuer{}
	r := newTestReconciler()
	r.GitCredentialIssuer = fake
	r.GitCredentialGuard = GitCredentialGuard{}

	if err := r.reconcileGitCredential(ctx, ws, "resource-no-guard"); err != nil {
		t.Fatalf("reconcileGitCredential with an empty Guard: %v, want success (an undeclared Guard must refuse nothing)", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("issuer called %d times, want 1", len(fake.requests))
	}
}

func TestReconcileGitCredential_UserProvidedSecretRefIsDefaultPath(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := createWorkspace(t, ctx, ns, "ws-user-secret", "https://git.example.com/acme/demo.git", "feature/user-secret", "default")
	secretName := "acme-demo-deploy-key"
	ws.Spec.GitCredentialSecretRef = &secretName
	if err := testClient.Update(ctx, ws); err != nil {
		t.Fatalf("set spec.gitCredentialSecretRef: %v", err)
	}

	userSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{"ssh-privatekey": []byte("caller-owned-key")},
	}
	if err := testClient.Create(ctx, userSecret); err != nil {
		t.Fatalf("create user-provided secret: %v", err)
	}

	fake := &fakeGitCredentialIssuer{}
	r := newTestReconciler()
	r.GitCredentialIssuer = fake

	if err := r.reconcileGitCredential(ctx, ws, "resource-user-secret"); err != nil {
		t.Fatalf("reconcileGitCredential: %v, want success from the referenced Secret alone", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("issuer was called %d times, want 0: a caller-provided reference must not trigger auto-issuance", len(fake.requests))
	}

	// The controller must not have copied or generated a Secret named after
	// resourceName: it references the caller's Secret in place.
	var generated corev1.Secret
	err := testClient.Get(ctx, types.NamespacedName{Name: "resource-user-secret", Namespace: ns}, &generated)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no generated Secret when a caller reference is provided, got err=%v", err)
	}
}

func TestReconcileGitCredential_MissingUserProvidedSecretFailsWithClearReason(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := createWorkspace(t, ctx, ns, "ws-missing-secret", "https://git.example.com/acme/demo.git", "feature/missing-secret", "default")
	secretName := "does-not-exist"
	ws.Spec.GitCredentialSecretRef = &secretName
	if err := testClient.Update(ctx, ws); err != nil {
		t.Fatalf("set spec.gitCredentialSecretRef: %v", err)
	}

	r := newTestReconciler()
	r.GitCredentialIssuer = &fakeGitCredentialIssuer{}

	err := r.reconcileGitCredential(ctx, ws, "resource-missing-secret")
	if err == nil {
		t.Fatal("reconcileGitCredential: expected an error for a missing referenced Secret, got nil")
	}
}

func TestReconcileGitCredential_PropagatesIssuerError(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := createWorkspace(t, ctx, ns, "ws-gitcred-err", "https://git.example.com/acme/demo.git", "feature/err", "default")

	r := newTestReconciler()
	r.GitCredentialIssuer = &fakeGitCredentialIssuer{err: errors.New("hosting API unavailable")}

	err := r.reconcileGitCredential(ctx, ws, "feature-err")
	if err == nil {
		t.Fatal("reconcileGitCredential: expected error to propagate from the issuer, got nil")
	}

	var secret corev1.Secret
	getErr := testClient.Get(ctx, types.NamespacedName{Name: "feature-err", Namespace: ns}, &secret)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected no Secret to be created on issuer failure, got err=%v", getErr)
	}
}

func TestReconcileGitCredential_NoReferenceAndNoIssuerFailsWithClearReason(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := createWorkspace(t, ctx, ns, "ws-no-issuer", "https://git.example.com/acme/demo.git", "feature/noissuer", "default")

	r := newTestReconciler()
	r.GitCredentialIssuer = nil

	if err := r.reconcileGitCredential(ctx, ws, "feature-noissuer"); err == nil {
		t.Fatal("reconcileGitCredential with no secret ref and nil GitCredentialIssuer: expected error, got nil")
	}
}
