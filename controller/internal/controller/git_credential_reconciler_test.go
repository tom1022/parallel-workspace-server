package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// fakeGitCredentialIssuer is the test double for the real Git hosting API,
// which this task deliberately does not implement (see git_credential_reconciler.go's
// package comment). It records every request so tests can assert on the
// scope the reconciler asked to be enforced, since the real hosting-side
// enforcement isn't observable from envtest.
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
		"giteaadmin/gitops-apps":                                 "giteaadmin/gitops-apps",
		"git@192.168.1.200:giteaadmin/gitops-apps.git":           "giteaadmin/gitops-apps",
		"https://gitea.fickledev.com/giteaadmin/gitops-apps.git": "giteaadmin/gitops-apps",
		"https://gitea.fickledev.com/giteaadmin/gitops-apps":     "giteaadmin/gitops-apps",
		"GiteaAdmin/Gitops-Apps":                                 "giteaadmin/gitops-apps",
		"https://gitea.fickledev.com/tom1022/demo.git":           "tom1022/demo",
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
	repo := "https://gitea.fickledev.com/tom1022/demo.git"
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
		t.Errorf("request.Repository = %q, want %q (14.12: scoped to the target repository only)", req.Repository, repo)
	}
	if req.AllowedBranch != branch {
		t.Errorf("request.AllowedBranch = %q, want %q (14.13: push limited to the working branch)", req.AllowedBranch, branch)
	}
	if !req.RejectDefaultBranchPush {
		t.Error("request.RejectDefaultBranchPush = false, want true (14.13: default branch push denied)")
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

func TestReconcileGitCredential_RefusesPlatformRepository(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	platformSpellings := []string{
		"giteaadmin/gitops-apps",
		"git@192.168.1.200:giteaadmin/gitops-apps.git",
		"https://gitea.fickledev.com/giteaadmin/gitops-apps.git",
	}
	for i, repo := range platformSpellings {
		name := "ws-platform-repo-guard"
		ws := createWorkspace(t, ctx, ns, name+string(rune('a'+i)), repo, "feature/x", "default")

		fake := &fakeGitCredentialIssuer{}
		r := newTestReconciler()
		r.GitCredentialIssuer = fake

		err := r.reconcileGitCredential(ctx, ws, "resource-"+string(rune('a'+i)))
		if err == nil {
			t.Fatalf("reconcileGitCredential(%q) = nil error, want a refusal (14.14: never grant write access to the platform repo)", repo)
		}
		if len(fake.requests) != 0 {
			t.Errorf("issuer was called for platform repository %q; it must never be reached", repo)
		}
	}
}

func TestReconcileGitCredential_PropagatesIssuerError(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := createWorkspace(t, ctx, ns, "ws-gitcred-err", "https://gitea.fickledev.com/tom1022/demo.git", "feature/err", "default")

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

func TestReconcileGitCredential_RequiresConfiguredIssuer(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := createWorkspace(t, ctx, ns, "ws-no-issuer", "https://gitea.fickledev.com/tom1022/demo.git", "feature/noissuer", "default")

	r := newTestReconciler()
	r.GitCredentialIssuer = nil

	if err := r.reconcileGitCredential(ctx, ws, "feature-noissuer"); err == nil {
		t.Fatal("reconcileGitCredential with nil GitCredentialIssuer: expected error, got nil")
	}
}
