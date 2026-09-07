package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/database"
)

func getDatabase(t *testing.T, ctx context.Context, ns, name string) *unstructured.Unstructured {
	t.Helper()
	db := database.CNPGDatabaseRef(ns, name)
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, db); err != nil {
		t.Fatalf("get Database %s: %v", name, err)
	}
	return db
}

func getDatabaseRole(t *testing.T, ctx context.Context, ns, name string) *unstructured.Unstructured {
	t.Helper()
	role := database.CNPGDatabaseRoleRef(ns, name)
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, role); err != nil {
		t.Fatalf("get DatabaseRole %s: %v", name, err)
	}
	return role
}

func TestReconcile_CreatesDatabaseAndDatabaseRole(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	createWorkspace(t, ctx, ns, "ws-db", "https://gitea.fickledev.com/tom1022/demo.git", "feature/db", "default")

	r := newTestReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-db", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	wantName := "feature-db"

	role := getDatabaseRole(t, ctx, ns, wantName)
	roleSpec, _, _ := unstructured.NestedMap(role.Object, "spec")
	if got, _, _ := unstructured.NestedString(roleSpec, "cluster", "name"); got != "devplatform-db" {
		t.Errorf("DatabaseRole spec.cluster.name = %q, want %q", got, "devplatform-db")
	}
	if got, _, _ := unstructured.NestedString(roleSpec, "name"); got != wantName {
		t.Errorf("DatabaseRole spec.name = %q, want %q", got, wantName)
	}
	if got, _, _ := unstructured.NestedBool(roleSpec, "login"); !got {
		t.Error("DatabaseRole spec.login = false, want true (required for clientCertificate)")
	}
	if got, _, _ := unstructured.NestedBool(roleSpec, "clientCertificate", "enabled"); !got {
		t.Error("DatabaseRole spec.clientCertificate.enabled = false, want true (8.2 auto-generated connection credential)")
	}
	if got, _, _ := unstructured.NestedString(roleSpec, "databaseRoleReclaimPolicy"); got != "delete" {
		t.Errorf("DatabaseRole spec.databaseRoleReclaimPolicy = %q, want %q", got, "delete")
	}
	if len(role.GetOwnerReferences()) != 1 || role.GetOwnerReferences()[0].Name != "ws-db" {
		t.Errorf("DatabaseRole ownerReferences = %+v, want a single reference to ws-db", role.GetOwnerReferences())
	}

	db := getDatabase(t, ctx, ns, wantName)
	dbSpec, _, _ := unstructured.NestedMap(db.Object, "spec")
	if got, _, _ := unstructured.NestedString(dbSpec, "cluster", "name"); got != "devplatform-db" {
		t.Errorf("Database spec.cluster.name = %q, want %q", got, "devplatform-db")
	}
	if got, _, _ := unstructured.NestedString(dbSpec, "name"); got != wantName {
		t.Errorf("Database spec.name = %q, want %q", got, wantName)
	}
	if got, _, _ := unstructured.NestedString(dbSpec, "owner"); got != wantName {
		t.Errorf("Database spec.owner = %q, want %q (its own dedicated DatabaseRole)", got, wantName)
	}
	if got, _, _ := unstructured.NestedString(dbSpec, "ensure"); got != "present" {
		t.Errorf("Database spec.ensure = %q, want %q", got, "present")
	}
	if got, _, _ := unstructured.NestedString(dbSpec, "databaseReclaimPolicy"); got != "delete" {
		t.Errorf("Database spec.databaseReclaimPolicy = %q, want %q", got, "delete")
	}
	if len(db.GetOwnerReferences()) != 1 || db.GetOwnerReferences()[0].Name != "ws-db" {
		t.Errorf("Database ownerReferences = %+v, want a single reference to ws-db", db.GetOwnerReferences())
	}
}

// TestReconcile_SuspendedLeavesDatabaseIntact locks in 8.10: idling a
// workspace (StatefulSet replicas 0/1, task 2.6) must never touch the
// Database/DatabaseRole. Reconcile's Suspended branch is a no-op today, which
// already satisfies this — this test exists so a future change to that branch
// cannot silently start deleting them.
func TestReconcile_SuspendedLeavesDatabaseIntact(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	createWorkspace(t, ctx, ns, "ws-suspend", "https://gitea.fickledev.com/tom1022/demo.git", "feature/suspend", "default")

	r := newTestReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-suspend", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	wantName := "feature-suspend"
	markStatefulSetReady(t, ctx, ns, wantName)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req.NamespacedName, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseSuspended
	if err := testClient.Status().Update(ctx, &ws); err != nil {
		t.Fatalf("force Suspended: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile while Suspended: %v", err)
	}

	getDatabase(t, ctx, ns, wantName)
	getDatabaseRole(t, ctx, ns, wantName)
}

// TestReconcile_FailsAndRollsBackDeletesDatabase extends the existing
// rollback path (workspace_controller_test.go) to cover 2.2's substrate:
// unlike the PVC, Database/DatabaseRole get no built-in Kubernetes deletion
// finalizer, so envtest (no CNPG operator, no kube-controller-manager) can
// assert they are actually gone, not merely marked for deletion.
func TestReconcile_FailsAndRollsBackDeletesDatabase(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	createWorkspace(t, ctx, ns, "ws-db-timeout", "https://gitea.fickledev.com/tom1022/demo.git", "feature/db-timeout", "default")

	r := newTestReconciler()
	r.ProvisioningTimeout = time.Nanosecond // already "elapsed" by the time Reconcile runs

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-db-timeout", Namespace: ns}}

	// reconcileDatabase creates the Database/DatabaseRole, then the same pass
	// fails waiting on the StatefulSet (timeout already elapsed), triggering
	// failAndRollback.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req.NamespacedName, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase = %q, want Failed", ws.Status.Phase)
	}
	resourceName := ws.Status.WorkspaceId

	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, database.CNPGDatabaseRef(ns, resourceName)); !apierrors.IsNotFound(err) {
		t.Errorf("expected Database %s deleted after rollback, get err = %v", resourceName, err)
	}
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, database.CNPGDatabaseRoleRef(ns, resourceName)); !apierrors.IsNotFound(err) {
		t.Errorf("expected DatabaseRole %s deleted after rollback, get err = %v", resourceName, err)
	}
}
