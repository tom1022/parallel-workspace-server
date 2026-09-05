// This file covers task 2.2: the branch-dedicated CNPG Database and
// DatabaseRole (design.md "Workspace Controller" Implementation Notes,
// Requirement 8.2/8.9/8.10). No typed Go API for CNPG is vendored into this
// module, so these are built as unstructured.Unstructured against the exact
// field names in the CRDs this cluster runs (apps/cnpg-operator/cnpg-operator.yaml).
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	cnpgAPIVersion       = "postgresql.cnpg.io/v1"
	cnpgDatabaseKind     = "Database"
	cnpgDatabaseRoleKind = "DatabaseRole"
)

func databaseRef(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(cnpgAPIVersion)
	u.SetKind(cnpgDatabaseKind)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

func databaseRoleRef(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(cnpgAPIVersion)
	u.SetKind(cnpgDatabaseRoleKind)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// buildDatabaseRole grants the branch its own login instead of sharing
// devplatform-db's superuser. login+clientCertificate.enabled makes CNPG
// auto-issue a TLS client certificate into a "<name>-client-cert" Secret
// (the CRD's own auto-generated connection credential mechanism); no
// passwordSecret is set (8.2).
func buildDatabaseRole(ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate, resourceName string) *unstructured.Unstructured {
	role := databaseRoleRef(ws.Namespace, resourceName)
	role.SetLabels(workspaceLabels(ws))
	role.Object["spec"] = map[string]interface{}{
		"cluster": map[string]interface{}{"name": tmpl.Spec.Database.ClusterRef},
		"name":    resourceName,
		"login":   true,
		"clientCertificate": map[string]interface{}{
			"enabled": true,
		},
		"ensure": "present",
		// A role reclaim policy of "delete" is required here, not just on the
		// Database: the Database's owner (below) references this role by
		// name, so leaving it "retain" would orphan a role no branch owns.
		"databaseRoleReclaimPolicy": "delete",
	}
	return role
}

// buildDatabase is owned by the DatabaseRole created above rather than the
// cluster superuser, so each branch's connection credential is scoped to its
// own database (8.2). databaseReclaimPolicy: delete is what makes destroying
// the Workspace (13.6/8.9) drop the real Postgres database, not just the k8s
// object.
func buildDatabase(ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate, resourceName string) *unstructured.Unstructured {
	db := databaseRef(ws.Namespace, resourceName)
	db.SetLabels(workspaceLabels(ws))
	db.Object["spec"] = map[string]interface{}{
		"cluster":               map[string]interface{}{"name": tmpl.Spec.Database.ClusterRef},
		"name":                  resourceName,
		"owner":                 resourceName,
		"ensure":                "present",
		"databaseReclaimPolicy": "delete",
	}
	return db
}

// reconcileDatabase creates the branch's Database/DatabaseRole if missing.
// Both carry an OwnerReference to ws (like the PVC/StatefulSet already do),
// so Kubernetes' own garbage collector deletes them only when ws itself is
// deleted — never on a phase transition to Suspended, which leaves ws alive
// and simply idles its StatefulSet (8.10). This reconciler does not wait for
// CNPG to report the Database "Applied": that belongs to DB Bootstrap (8.5-8.8).
func (r *WorkspaceReconciler) reconcileDatabase(ctx context.Context, ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate, resourceName string) error {
	role := buildDatabaseRole(ws, tmpl, resourceName)
	if err := controllerutil.SetControllerReference(ws, role, r.Scheme); err != nil {
		return err
	}
	if err := r.ensureCreated(ctx, role); err != nil {
		return err
	}

	db := buildDatabase(ws, tmpl, resourceName)
	if err := controllerutil.SetControllerReference(ws, db, r.Scheme); err != nil {
		return err
	}
	return r.ensureCreated(ctx, db)
}
