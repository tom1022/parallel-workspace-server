// This file covers task 2.4: issuing each workspace a Git credential scoped
// to its target repository (design.md "Workspace Controller" Implementation
// Notes, Requirement 14.12/14.13/14.14/20.1). This cluster's Git remote is a
// self-hosted Gitea instance (git@192.168.1.200); which concrete mechanism
// issues a repository- and branch-scoped credential there — a deploy key
// plus branch protection, a fine-grained token, or a pre-receive hook policy
// — is an operational decision not yet made, so no such client is vendored
// here. GitCredentialIssuer is the boundary: this reconciler only decides
// *what* scope to request and *where* to store the result, never how the
// hosting side enforces it.
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

// platformRepository identifies this repository (gitops-apps) itself, in the
// normalized "owner/repo" form normalizeRepositoryIdentity produces. A
// Workspace can never target it: this platform's own configuration is
// GitOps-managed, not something an autonomous workspace edits (14.14).
const platformRepository = "giteaadmin/gitops-apps"

// gitCredentialHandleAnnotation preserves the issuer's hosting-side handle
// (e.g. a deploy key ID) on the Secret it backs, so a future revoke path
// (workspace destroy, out of this task's scope) has what it needs without
// re-deriving it.
const gitCredentialHandleAnnotation = "devplatform.fickledev.com/git-credential-handle"

// GitCredentialRequest is the scope a workspace's Git credential must be
// limited to. GitCredentialIssuer implementations are trusted to enforce
// these at the hosting side; this struct is the contract a fake/mock can
// assert against in tests since the real enforcement is not observable from
// envtest.
type GitCredentialRequest struct {
	// WorkspaceId names the credential for idempotent issuance.
	WorkspaceId string
	// Repository is the sole remote the credential may reach (14.12).
	Repository string
	// AllowedBranch is the sole branch the credential may push to (14.13).
	AllowedBranch string
	// RejectDefaultBranchPush is always true: it names the constraint the
	// issuer must additionally enforce beyond AllowedBranch — a push to
	// Repository's own default branch is denied even if an operator ever
	// sets AllowedBranch to that name (14.13).
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

// GitCredentialIssuer is the boundary to the platform's Git hosting API.
// Production wiring (main.go) injects the concrete Gitea-backed
// implementation once the issuance mechanism is decided; tests inject a
// fake.
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

// reconcileGitCredential issues ws's Git credential Secret if it does not
// already exist. The repository guard runs regardless of what the injected
// Issuer would do: it is this controller's own guarantee that no credential
// it stores can reach the platform's configuration repository (14.14),
// independent of the (currently undetermined) hosting-side mechanism.
func (r *WorkspaceReconciler) reconcileGitCredential(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string) error {
	if normalizeRepositoryIdentity(ws.Spec.Repository) == platformRepository {
		return fmt.Errorf("devplatform: refusing to issue a git credential for the platform's own repository %q", ws.Spec.Repository)
	}
	if r.GitCredentialIssuer == nil {
		return fmt.Errorf("devplatform: no GitCredentialIssuer configured")
	}

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
