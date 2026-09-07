package database

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	cnpgAPIVersion       = "postgresql.cnpg.io/v1"
	cnpgDatabaseKind     = "Database"
	cnpgDatabaseRoleKind = "DatabaseRole"
)

// CNPGAdapter generates a CloudNativePG Database and DatabaseRole for the
// branch (Requirement 8.1/8.2). No typed Go API for CNPG is vendored into
// this module, so these are built as unstructured.Unstructured against the
// exact field names in the CRDs the target cluster runs.
type CNPGAdapter struct {
	Client client.Client
	Scheme *runtime.Scheme
}

var _ DatabaseAdapter = (*CNPGAdapter)(nil)

// CNPGDatabaseRef and CNPGDatabaseRoleRef return empty CNPG Database/
// DatabaseRole references suitable for client.Get/Delete by namespace/name,
// mirroring routing.TraefikIngressRouteRef — exported so callers (tests,
// this adapter's own Release) can look a generated object up without this
// package exposing anything richer than "here is the kind and name".
func CNPGDatabaseRef(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(cnpgAPIVersion)
	u.SetKind(cnpgDatabaseKind)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

func CNPGDatabaseRoleRef(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(cnpgAPIVersion)
	u.SetKind(cnpgDatabaseRoleKind)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// buildDatabaseRole grants the branch its own login instead of sharing the
// cluster's superuser. login+clientCertificate.enabled makes CNPG
// auto-issue a TLS client certificate into a "<name>-client-cert" Secret
// (the CRD's own auto-generated connection credential mechanism); no
// passwordSecret is set (8.2).
func buildDatabaseRole(target DatabaseTarget) *unstructured.Unstructured {
	role := CNPGDatabaseRoleRef(target.Namespace, target.Name)
	role.SetLabels(target.Labels)
	role.Object["spec"] = map[string]interface{}{
		"cluster": map[string]interface{}{"name": target.ClusterRef},
		"name":    target.Name,
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
func buildDatabase(target DatabaseTarget) *unstructured.Unstructured {
	db := CNPGDatabaseRef(target.Namespace, target.Name)
	db.SetLabels(target.Labels)
	db.Object["spec"] = map[string]interface{}{
		"cluster":               map[string]interface{}{"name": target.ClusterRef},
		"name":                  target.Name,
		"owner":                 target.Name,
		"ensure":                "present",
		"databaseReclaimPolicy": "delete",
	}
	return db
}

// Ensure creates the branch's Database/DatabaseRole if missing. Both carry
// an OwnerReference to the Workspace (like the PVC/StatefulSet already do),
// so Kubernetes' own garbage collector deletes them only when the Workspace
// itself is deleted — never on a phase transition to Suspended (8.10). This
// does not wait for CNPG to report the Database "Applied": that belongs to
// DB Bootstrap (8.5-8.8).
func (a *CNPGAdapter) Ensure(ctx context.Context, target DatabaseTarget) (DatabaseRef, error) {
	role := buildDatabaseRole(target)
	if err := controllerutil.SetControllerReference(ownerStub(target), role, a.Scheme); err != nil {
		return DatabaseRef{}, err
	}
	if err := ensureCreated(ctx, a.Client, role); err != nil {
		return DatabaseRef{}, fmt.Errorf("ensure DatabaseRole %s: %w", role.GetName(), err)
	}

	db := buildDatabase(target)
	if err := controllerutil.SetControllerReference(ownerStub(target), db, a.Scheme); err != nil {
		return DatabaseRef{}, err
	}
	if err := ensureCreated(ctx, a.Client, db); err != nil {
		return DatabaseRef{}, fmt.Errorf("ensure Database %s: %w", db.GetName(), err)
	}
	return DatabaseRef{}, nil
}

// Release deletes the branch's Database/DatabaseRole. Both get no built-in
// Kubernetes deletion finalizer, so a rollback that leaves the Workspace
// alive (failAndRollback) has to delete them explicitly rather than relying
// on OwnerReference GC.
func (a *CNPGAdapter) Release(ctx context.Context, target DatabaseTarget) error {
	if err := client.IgnoreNotFound(a.Client.Delete(ctx, CNPGDatabaseRef(target.Namespace, target.Name))); err != nil {
		return err
	}
	if err := client.IgnoreNotFound(a.Client.Delete(ctx, CNPGDatabaseRoleRef(target.Namespace, target.Name))); err != nil {
		return err
	}
	return nil
}
