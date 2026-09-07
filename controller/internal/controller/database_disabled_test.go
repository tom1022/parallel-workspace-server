// This file covers task 3.4's disabled path: a deployment that opts out of
// the branch-dedicated database entirely (design.md "Database Adapter",
// Requirement 3.5) still provisions a usable workspace, with no CNPG
// resources and no database-shaped volume/env/init container on its
// StatefulSet.
package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/database"
)

// createTemplateWithoutDatabase mirrors createTemplate but leaves
// Spec.Database nil, matching what templates/workspacetemplate.yaml renders
// when workspaceTemplate.database.enabled is false.
func createTemplateWithoutDatabase(t *testing.T, ctx context.Context, ns, name string) {
	t.Helper()
	tmpl := &devplatformv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: devplatformv1alpha1.WorkspaceTemplateSpec{
			Image: "busybox:1.36",
			Resources: devplatformv1alpha1.WorkspaceResources{
				Requests: devplatformv1alpha1.ResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   devplatformv1alpha1.ResourceList{CPU: "500m", Memory: "256Mi"},
			},
			Storage:  devplatformv1alpha1.WorkspaceStorage{Size: "1Gi"},
			NodeName: "test-node",
			Auth:     devplatformv1alpha1.WorkspaceAuthRef{SecretRef: "claude-auth"},
			Evacuation: devplatformv1alpha1.WorkspaceEvacuation{
				Bucket:    "workspace",
				Endpoint:  "http://garage.garage.svc.cluster.local:3900",
				Region:    "garage",
				SecretRef: "garage-evacuation-credentials",
			},
		},
	}
	if err := testClient.Create(ctx, tmpl); err != nil {
		t.Fatalf("create WorkspaceTemplate: %v", err)
	}
	ensureStagedNode(t, ctx, tmpl.Spec.NodeName, tmpl.Spec.Image)
}

// TestReconcile_NoDatabaseReachesReadyWithoutCNPGResources is the acceptance
// scenario for task 3.4: a deployment wired with NoopAdapter and a template
// that configures no database still reaches Ready, with neither a Database
// nor a DatabaseRole ever created and no database-shaped resource on the Pod.
func TestReconcile_NoDatabaseReachesReadyWithoutCNPGResources(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplateWithoutDatabase(t, ctx, ns, "no-db")
	createWorkspace(t, ctx, ns, "ws-no-db", "https://gitea.fickledev.com/tom1022/demo.git", "feature/no-db", "no-db")

	r := newTestReconciler()
	// The deployment-wide kill switch (main.go's DATABASE_TYPE=none) —
	// exercised together with the per-template nil above, matching how the
	// chart wires both from the same workspaceTemplate.database.enabled
	// toggle.
	r.DatabaseAdapter = &database.NoopAdapter{}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-no-db", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	wantID := "feature-no-db"
	if err := testClient.Get(ctx, types.NamespacedName{Name: wantID, Namespace: ns}, database.CNPGDatabaseRef(ns, wantID)); !apierrors.IsNotFound(err) {
		t.Errorf("Database %s exists (or unexpected error %v), want none created", wantID, err)
	}
	if err := testClient.Get(ctx, types.NamespacedName{Name: wantID, Namespace: ns}, database.CNPGDatabaseRoleRef(ns, wantID)); !apierrors.IsNotFound(err) {
		t.Errorf("DatabaseRole %s exists (or unexpected error %v), want none created", wantID, err)
	}

	markStatefulSetReady(t, ctx, ns, wantID)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req.NamespacedName, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready", ws.Status.Phase)
	}

	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: wantID, Namespace: ns}, &sts); err != nil {
		t.Fatalf("get StatefulSet: %v", err)
	}
	if len(sts.Spec.Template.Spec.InitContainers) != 1 {
		t.Errorf("init containers = %d, want only the checkout (no db-bootstrap)", len(sts.Spec.Template.Spec.InitContainers))
	}
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == databaseCertVolumeName {
			t.Errorf("volumes = %+v, want no client certificate volume", sts.Spec.Template.Spec.Volumes)
		}
	}
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "PGHOST" {
			t.Errorf("PGHOST = %q, want no database env", e.Value)
		}
	}
}
