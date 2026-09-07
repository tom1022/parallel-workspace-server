// This file covers task 3.4: making the branch-dedicated database optional
// through a pluggable DatabaseAdapter (design.md "Database Adapter",
// Requirement 3.5, internal/adapter/database), mirroring task 3.1's
// RoutingAdapter. The reconciler asks the adapter for "the branch database"
// without knowing whether it generates CNPG resources or nothing at all. A
// WorkspaceTemplate with no database configured (Spec.Database == nil) skips
// the adapter entirely — resources.go reads the same nil to omit the
// database volume/env/init container from the StatefulSet.
package controller

import (
	"context"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/database"
)

// databaseTarget builds the DatabaseAdapter request for ws/resourceName.
// Release only reads Namespace/Name (see DatabaseTarget), so callers that
// only need to identify already-generated resources for cleanup
// (failAndRollback) can use this with clusterRef left empty, even when
// Ensure was never called for this workspace.
func (r *WorkspaceReconciler) databaseTarget(ws *devplatformv1alpha1.Workspace, resourceName, clusterRef string) database.DatabaseTarget {
	return database.DatabaseTarget{
		WorkspaceName: ws.Name,
		WorkspaceUID:  ws.UID,
		Namespace:     ws.Namespace,
		Name:          resourceName,
		ClusterRef:    clusterRef,
		Labels:        workspaceLabels(ws),
	}
}

// reconcileDatabase provisions the branch's database through whichever
// DatabaseAdapter this deployment selected, unless tmpl opts the template
// out of having one at all (Requirement 3.5).
func (r *WorkspaceReconciler) reconcileDatabase(ctx context.Context, ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate, resourceName string) error {
	if tmpl.Spec.Database == nil {
		return nil
	}
	_, err := r.DatabaseAdapter.Ensure(ctx, r.databaseTarget(ws, resourceName, tmpl.Spec.Database.ClusterRef))
	return err
}
