// This file covers Git credential handling for a Workspace (Requirements
// 5.1-5.6). Two concerns are kept separate: GitCredentialGuard decides what a
// workspace may never target, independent of how a credential is obtained;
// the user-provided Secret reference and GitCredentialIssuer decide how one
// is obtained. GitCredentialIssuer is the boundary to a concrete Git hosting
// API a deployment may optionally wire in; issuing a repository- and
// branch-scoped credential there (a deploy key plus branch protection, a
// fine-grained token, a pre-receive hook policy) is that deployment's
// decision, not this reconciler's.
package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// GitCredentialGuard names what a deployment declares off limits for any
// Workspace's Git credential, regardless of which mechanism supplies the
// credential (5.5, 5.6). Every field is deployment-supplied configuration;
// this package holds no repository or branch name of its own, so an empty
// Guard protects nothing rather than falling back to a built-in default
// (design.md 525行).
type GitCredentialGuard struct {
	// ProtectedRepositories are repositories, normalized via
	// normalizeRepositoryIdentity, that a Workspace may never target. A
	// deployment lists its own GitOps configuration repository here so no
	// credential path can be issued or referenced against it.
	ProtectedRepositories []string
	// ProtectedBranches are branch names a Workspace may never be scoped to
	// (typically the repository's default branch names).
	ProtectedBranches []string
}

// refuses reports whether repository/branch fall inside g's declared limits.
func (g GitCredentialGuard) refuses(repository, branch string) (string, bool) {
	normalized := normalizeRepositoryIdentity(repository)
	for _, protected := range g.ProtectedRepositories {
		if normalizeRepositoryIdentity(protected) == normalized {
			return fmt.Sprintf("devplatform: refusing to issue a git credential for protected repository %q", repository), true
		}
	}
	for _, protected := range g.ProtectedBranches {
		if strings.EqualFold(protected, branch) {
			return fmt.Sprintf("devplatform: refusing to issue a git credential that could push to protected branch %q", branch), true
		}
	}
	return "", false
}

// gitCredentialHandleAnnotation preserves an issuer's hosting-side handle
// (e.g. a deploy key ID) on the Secret it backs, so a future revoke path
// (workspace destroy, out of this task's scope) has what it needs without
// re-deriving it.
const gitCredentialHandleAnnotation = "workspace.tom1022.github.io/git-credential-handle"

// GitCredentialRequest is the scope a workspace's Git credential must be
// limited to. GitCredentialIssuer implementations are trusted to enforce
// these at the hosting side; this struct is the contract a fake/mock can
// assert against in tests since the real enforcement is not observable from
// envtest.
type GitCredentialRequest struct {
	// WorkspaceId names the credential for idempotent issuance.
	WorkspaceId string
	// Repository is the sole remote the credential may reach (5.1).
	Repository string
	// AllowedBranch is the sole branch the credential may push to.
	AllowedBranch string
	// RejectDefaultBranchPush is always true: it names the constraint the
	// issuer must additionally enforce beyond AllowedBranch.
	RejectDefaultBranchPush bool
}

// IssuedGitCredential is what a GitCredentialIssuer hands back: Secret data
// ready to store verbatim (an SSH deploy key's private half, a scoped
// token — whatever the concrete mechanism produces) plus the hosting-side
// handle for future revocation.
type IssuedGitCredential struct {
	SecretData map[string][]byte
	SecretType corev1.SecretType
	Handle     string
}

// GitCredentialIssuer is the optional extension point that auto-issues a Git
// credential (5.4). A deployment without one relies solely on
// Workspace.Spec.GitCredentialSecretRef.
type GitCredentialIssuer interface {
	Issue(ctx context.Context, req GitCredentialRequest) (IssuedGitCredential, error)
}

// normalizeRepositoryIdentity reduces any of this platform's accepted
// repository spellings (SSH shorthand, HTTPS URL, bare "owner/repo") to a
// lowercase "owner/repo" so they compare equal regardless of how a caller
// wrote Workspace.spec.repository.
func normalizeRepositoryIdentity(repo string) string {
	repo = strings.TrimSuffix(strings.TrimSpace(repo), ".git")
	repo = strings.ReplaceAll(repo, ":", "/") // "git@host:owner/repo" -> ".../owner/repo"
	parts := strings.Split(repo, "/")
	if len(parts) < 2 {
		return strings.ToLower(repo)
	}
	return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1])
}

// reconcileGitCredential ensures ws can reach a Git credential Secret,
// either by trusting a caller-provided reference (5.1-5.3, the default path)
// or, when none is given, by auto-issuing one through GitCredentialIssuer
// (5.4). The Guard check runs first and unconditionally, so neither path can
// bypass it (5.5, 5.6).
func (r *WorkspaceReconciler) reconcileGitCredential(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string) error {
	branch := strings.TrimSpace(ws.Spec.Branch)
	if reason, refused := r.GitCredentialGuard.refuses(ws.Spec.Repository, branch); refused {
		return fmt.Errorf("%s", reason)
	}

	if ws.Spec.GitCredentialSecretRef != nil {
		return r.verifyUserProvidedGitCredential(ctx, ws, *ws.Spec.GitCredentialSecretRef)
	}

	if r.GitCredentialIssuer == nil {
		return fmt.Errorf("devplatform: git credential unavailable: spec.gitCredentialSecretRef is not set and no GitCredentialIssuer is configured")
	}
	return r.issueGitCredential(ctx, ws, resourceName)
}

// verifyUserProvidedGitCredential confirms the Secret the caller named
// exists in ws's namespace. The controller neither copies nor mutates it: it
// is the caller's own resource, referenced in place (5.1, 5.2).
func (r *WorkspaceReconciler) verifyUserProvidedGitCredential(ctx context.Context, ws *devplatformv1alpha1.Workspace, secretName string) error {
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ws.Namespace}, &secret)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("devplatform: git credential secret %q (spec.gitCredentialSecretRef) not found in namespace %q", secretName, ws.Namespace)
	}
	return err
}

func (r *WorkspaceReconciler) issueGitCredential(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string) error {
	// Issuance may be a real external call; unlike the k8s-only reconcilers
	// this file's siblings write, re-running it on every provisioning pass
	// would re-issue credentials pointlessly, so check first.
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ws.Namespace}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	issued, err := r.GitCredentialIssuer.Issue(ctx, GitCredentialRequest{
		WorkspaceId:             resourceName,
		Repository:              ws.Spec.Repository,
		AllowedBranch:           ws.Spec.Branch,
		RejectDefaultBranchPush: true,
	})
	if err != nil {
		return err
	}

	secretType := issued.SecretType
	if secretType == "" {
		secretType = corev1.SecretTypeOpaque
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: ws.Namespace,
			Labels:    workspaceLabels(ws),
		},
		Type: secretType,
		Data: issued.SecretData,
	}
	if issued.Handle != "" {
		secret.Annotations = map[string]string{gitCredentialHandleAnnotation: issued.Handle}
	}
	if err := controllerutil.SetControllerReference(ws, secret, r.Scheme); err != nil {
		return err
	}
	return r.ensureCreated(ctx, secret)
}
