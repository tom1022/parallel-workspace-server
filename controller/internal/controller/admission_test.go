package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// assertHeldForResourceWaiting is the shared assertion for all three
// admission gates: the workspace must not fail, must not gain any substrate,
// and must carry the ResourceWaiting condition the acceptance criterion
// (15.6/15.11) requires the requester be notified with.
func assertHeldForResourceWaiting(t *testing.T, ctx context.Context, ns, wsName, wantReason string) {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, types.NamespacedName{Name: wsName, Namespace: ns}, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase == devplatformv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase = Failed, want held in Provisioning (resource limits must not fail provisioning)")
	}

	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == conditionResourceWaiting && c.Status == metav1.ConditionTrue {
			found = true
			if c.Reason != wantReason {
				t.Errorf("ResourceWaiting reason = %q, want %q", c.Reason, wantReason)
			}
		}
	}
	if !found {
		t.Fatalf("expected a ResourceWaiting=True condition, got %+v", ws.Status.Conditions)
	}

	var pvc corev1.PersistentVolumeClaim
	err := testClient.Get(ctx, types.NamespacedName{Name: wsName, Namespace: ns}, &pvc)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no PVC to be created while held for resource limits, got err=%v", err)
	}
}

func TestReconcile_HoldsForImageNotStaged(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	// A template targeting a node whose image pre-stage never completed
	// (unlike createTemplate's "test-node", ensureStagedNode is deliberately
	// not called here).
	tmpl := &devplatformv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "unstaged", Namespace: ns},
		Spec: devplatformv1alpha1.WorkspaceTemplateSpec{
			Image: "busybox:1.36",
			Resources: devplatformv1alpha1.WorkspaceResources{
				Requests: devplatformv1alpha1.ResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   devplatformv1alpha1.ResourceList{CPU: "500m", Memory: "256Mi"},
			},
			Storage:    devplatformv1alpha1.WorkspaceStorage{Size: "1Gi"},
			NodeName:   "node-without-prestage",
			Database:   devplatformv1alpha1.WorkspaceDatabaseRef{ClusterRef: "devplatform-db"},
			Auth:       devplatformv1alpha1.WorkspaceAuthRef{SecretRef: "claude-auth"},
			Evacuation: devplatformv1alpha1.WorkspaceEvacuation{Bucket: "workspace", Endpoint: "http://garage.garage.svc.cluster.local:3900", Region: "garage", SecretRef: "garage-evacuation-credentials"},
		},
	}
	if err := testClient.Create(ctx, tmpl); err != nil {
		t.Fatalf("create WorkspaceTemplate: %v", err)
	}

	createWorkspace(t, ctx, ns, "ws-unstaged", "https://gitea.fickledev.com/tom1022/demo.git", "feature/unstaged", "unstaged")

	r := newTestReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-unstaged", Namespace: ns}}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Errorf("expected a requeue while held, got %+v", result)
	}

	assertHeldForResourceWaiting(t, ctx, ns, "ws-unstaged", "ImageNotStaged")
}

func TestReconcile_HoldsForResourceQuotaExceeded(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")

	// Simulate a namespace whose ResourceQuota (workspace-quota.yaml) already
	// has no headroom left for requests.cpu; envtest doesn't run the
	// resourcequota controller, so Status is set directly the way it would
	// converge to in a real cluster.
	rq := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceResourceQuotaName, Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{"requests.cpu": resource.MustParse("100m")}},
	}
	if err := testClient.Create(ctx, rq); err != nil {
		t.Fatalf("create ResourceQuota: %v", err)
	}
	rq.Status = corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{"requests.cpu": resource.MustParse("100m")},
		Used: corev1.ResourceList{"requests.cpu": resource.MustParse("100m")},
	}
	if err := testClient.Status().Update(ctx, rq); err != nil {
		t.Fatalf("set ResourceQuota status: %v", err)
	}

	createWorkspace(t, ctx, ns, "ws-quota", "https://gitea.fickledev.com/tom1022/demo.git", "feature/quota", "default")

	r := newTestReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-quota", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertHeldForResourceWaiting(t, ctx, ns, "ws-quota", "QuotaExceeded")
}

func TestReconcile_HoldsForNodeDiskBudgetExceeded(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	tmpl := &devplatformv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tight-disk", Namespace: ns},
		Spec: devplatformv1alpha1.WorkspaceTemplateSpec{
			Image: "busybox:1.36",
			Resources: devplatformv1alpha1.WorkspaceResources{
				Requests: devplatformv1alpha1.ResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   devplatformv1alpha1.ResourceList{CPU: "500m", Memory: "256Mi"},
			},
			// Size alone already exceeds the budget: blocks even before any
			// other workspace on the node is considered.
			Storage:    devplatformv1alpha1.WorkspaceStorage{Size: "1Gi", NodeDiskBudget: "500Mi"},
			NodeName:   "test-node",
			Database:   devplatformv1alpha1.WorkspaceDatabaseRef{ClusterRef: "devplatform-db"},
			Auth:       devplatformv1alpha1.WorkspaceAuthRef{SecretRef: "claude-auth"},
			Evacuation: devplatformv1alpha1.WorkspaceEvacuation{Bucket: "workspace", Endpoint: "http://garage.garage.svc.cluster.local:3900", Region: "garage", SecretRef: "garage-evacuation-credentials"},
		},
	}
	if err := testClient.Create(ctx, tmpl); err != nil {
		t.Fatalf("create WorkspaceTemplate: %v", err)
	}
	ensureStagedNode(t, ctx, tmpl.Spec.NodeName, tmpl.Spec.Image)

	createWorkspace(t, ctx, ns, "ws-disk", "https://gitea.fickledev.com/tom1022/demo.git", "feature/disk", "tight-disk")

	r := newTestReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-disk", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertHeldForResourceWaiting(t, ctx, ns, "ws-disk", "QuotaExceeded")
}
