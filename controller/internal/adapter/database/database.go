// Package database provisions the branch-dedicated database backing one
// workspace (design.md "Database Adapter", Requirement 3.5). The Workspace
// Controller depends only on the DatabaseAdapter interface below; it never
// learns whether the implementation selected at deploy time creates
// CNPG-specific resources or nothing at all.
package database

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// DatabaseAdapter provisions and releases the per-branch database for one
// workspace. A no-op implementation satisfies deployments without one
// (Requirement 3.5).
//
// Ensure is idempotent, mirroring routing.RoutingAdapter's contract: calling
// it again for the same DatabaseTarget converges on the same resources
// instead of erroring or duplicating them.
type DatabaseAdapter interface {
	Ensure(ctx context.Context, target DatabaseTarget) (DatabaseRef, error)
	Release(ctx context.Context, target DatabaseTarget) error
}

// DatabaseTarget describes the branch database one workspace needs.
type DatabaseTarget struct {
	// WorkspaceName and WorkspaceUID identify the owning Workspace, so
	// implementations can set an OwnerReference back to it without a round
	// trip to fetch the object the caller already has loaded (mirrors
	// routing.RoutingTarget).
	WorkspaceName string
	WorkspaceUID  types.UID
	Namespace     string
	// Name is both the branch role/database name and the key Release looks
	// generated resources up by — Release reads only Namespace/Name, so a
	// caller cleaning up after a rollback (ClusterRef unknown or irrelevant)
	// can still find them.
	Name string
	// ClusterRef names the database cluster/instance to provision against.
	// Only Ensure reads it.
	ClusterRef string
	// Labels are applied to every resource generated for this target,
	// matching the labels the reconciler already stamps on the rest of a
	// workspace's owned substrate.
	Labels map[string]string
}

// DatabaseRef is what Ensure provisioned. It carries no fields today: no
// caller needs anything from it beyond "did Ensure succeed" (the error
// return already covers that) — resources.go still derives the branch's
// connection details from WorkspaceTemplateSpec.Database directly. The type
// exists so this interface matches design.md's shape and can grow
// connection details later without an interface change.
type DatabaseRef struct{}

// ownerStub builds the minimal Workspace object controllerutil.SetControllerReference
// needs: it only reads Name/Namespace/UID plus the scheme-resolved GVK, so a
// full Get of the real object is unnecessary.
func ownerStub(target DatabaseTarget) *devplatformv1alpha1.Workspace {
	return &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      target.WorkspaceName,
			Namespace: target.Namespace,
			UID:       target.WorkspaceUID,
		},
	}
}

// ensureCreated makes obj's creation idempotent: a caller that runs Ensure
// again after the object already exists must not error (DatabaseAdapter's
// idempotence contract).
func ensureCreated(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
