package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

func newTestReconciler() *WorkspaceReconciler {
	return &WorkspaceReconciler{
		Client:              testClient,
		Scheme:              scheme.Scheme,
		ProvisioningTimeout: time.Hour, // tests override per-case when they need the timeout path
		PollInterval:        10 * time.Millisecond,
		// Tests unrelated to git credential issuance don't want to fail
		// provisioning over an unconfigured issuer; git_credential_reconciler_test.go
		// swaps this out where the issuer's behavior is itself under test.
		GitCredentialIssuer: &fakeGitCredentialIssuer{},
		// Same reasoning as GitCredentialIssuer above: most tests don't care
		// about evacuation and shouldn't fail destroy over an unconfigured
		// confirmer; evacuation_test.go swaps this out where it is itself
		// under test.
		EvacuationConfirmer: &fakeEvacuationConfirmer{complete: true},
		// Likewise: tests that are not about evacuation must not fail a
		// suspend or destroy over an unreachable supervisor.
		EvacuationRequester: &fakeEvacuationRequester{},
	}
}

func createTemplate(t *testing.T, ctx context.Context, ns, name string) {
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
			Database: devplatformv1alpha1.WorkspaceDatabaseRef{ClusterRef: "devplatform-db"},
			Auth:     devplatformv1alpha1.WorkspaceAuthRef{SecretRef: "claude-auth"},
			Evacuation: devplatformv1alpha1.WorkspaceEvacuation{
				Bucket:    "workspace",
				SecretRef: "garage-evacuation-credentials",
			},
		},
	}
	if err := testClient.Create(ctx, tmpl); err != nil {
		t.Fatalf("create WorkspaceTemplate: %v", err)
	}
	// Every existing test exercises the golden path, which now also requires
	// the target node to have the template's image pre-staged (15.4/task 2.5);
	// admission_test.go covers the not-staged path explicitly against its own
	// node/image, so it doesn't touch this one.
	ensureStagedNode(t, ctx, tmpl.Spec.NodeName, tmpl.Spec.Image)
}

// ensureStagedNode creates (or updates) the cluster-scoped Node object with
// image staged in its status.images, matching what image-prestage-daemonset.yaml
// achieves in a real cluster once its Pod is Running on that node. Node is
// shared across every test in this package (no per-namespace isolation like
// newNamespace gives other fixtures), so this is idempotent: it only adds the
// image if not already present.
func ensureStagedNode(t *testing.T, ctx context.Context, nodeName, image string) {
	t.Helper()
	var node corev1.Node
	err := testClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node)
	if apierrors.IsNotFound(err) {
		node = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
		if err := testClient.Create(ctx, &node); err != nil {
			t.Fatalf("create Node %s: %v", nodeName, err)
		}
	} else if err != nil {
		t.Fatalf("get Node %s: %v", nodeName, err)
	}

	// envtest runs no kubelet, so nothing posts the Ready condition every real
	// Node carries. Without it the node reads as unavailable to 13.9's check.
	setNodeReady(t, ctx, nodeName, true)
	if err := testClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("get Node %s: %v", nodeName, err)
	}

	for _, img := range node.Status.Images {
		for _, name := range img.Names {
			if name == image {
				return
			}
		}
	}
	node.Status.Images = append(node.Status.Images, corev1.ContainerImage{Names: []string{image}})
	if err := testClient.Status().Update(ctx, &node); err != nil {
		t.Fatalf("stage image %s on Node %s: %v", image, nodeName, err)
	}
}

func createWorkspace(t *testing.T, ctx context.Context, ns, name, repo, branch, templateRef string) *devplatformv1alpha1.Workspace {
	t.Helper()
	ws := &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: devplatformv1alpha1.WorkspaceSpec{
			Repository:  repo,
			Branch:      branch,
			TemplateRef: templateRef,
		},
	}
	if err := testClient.Create(ctx, ws); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	return ws
}

func markStatefulSetReady(t *testing.T, ctx context.Context, ns, name string) {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &sts); err != nil {
		t.Fatalf("get StatefulSet %s: %v", name, err)
	}
	sts.Status.ReadyReplicas = 1
	sts.Status.Replicas = 1
	if err := testClient.Status().Update(ctx, &sts); err != nil {
		t.Fatalf("mark StatefulSet %s ready: %v", name, err)
	}
}

func TestReconcile_ProvisionsAndReachesReady(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	createWorkspace(t, ctx, ns, "ws-under-test", "https://gitea.fickledev.com/tom1022/demo.git", "feature/hello", "default")

	r := newTestReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-under-test", Namespace: ns}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req.NamespacedName, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseProvisioning && ws.Status.Phase != "" {
		t.Errorf("expected phase to still be Provisioning, got %q", ws.Status.Phase)
	}
	wantID := "feature-hello"
	if ws.Status.WorkspaceId != wantID {
		t.Errorf("workspaceId = %q, want %q (DNS-normalized branch)", ws.Status.WorkspaceId, wantID)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: wantID, Namespace: ns}, &pvc); err != nil {
		t.Fatalf("expected PVC %s to be created: %v", wantID, err)
	}
	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: wantID, Namespace: ns}, &sts); err != nil {
		t.Fatalf("expected StatefulSet %s to be created: %v", wantID, err)
	}

	// No kubelet in envtest: simulate the pod becoming ready, then reconcile again.
	markStatefulSetReady(t, ctx, ns, wantID)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	if err := testClient.Get(ctx, req.NamespacedName, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready", ws.Status.Phase)
	}
	if ws.Status.WorkspaceId == "" {
		t.Error("workspaceId is empty at Ready")
	}
	if ws.Status.Urls.Session == "" {
		t.Error("urls.session is empty at Ready")
	}
	if ws.Status.SessionId == "" {
		t.Error("sessionId is empty at Ready")
	}
	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == conditionProvisioned && c.Status == metav1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Error("expected a Provisioned=True condition")
	}
}

func TestReconcile_DuplicateBranchReusesExistingWorkspace(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	repo := "https://gitea.fickledev.com/tom1022/demo.git"
	branch := "feature/dup"

	createWorkspace(t, ctx, ns, "ws-first", repo, branch, "default")
	r := newTestReconciler()

	firstReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-first", Namespace: ns}}
	if _, err := r.Reconcile(ctx, firstReq); err != nil {
		t.Fatalf("reconcile ws-first: %v", err)
	}
	markStatefulSetReady(t, ctx, ns, "feature-dup")
	if _, err := r.Reconcile(ctx, firstReq); err != nil {
		t.Fatalf("reconcile ws-first (ready pass): %v", err)
	}

	// A second Workspace object requesting the same repository+branch must not
	// provision its own substrate (1.6): it should mirror ws-first instead.
	createWorkspace(t, ctx, ns, "ws-second", repo, branch, "default")
	secondReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-second", Namespace: ns}}
	if _, err := r.Reconcile(ctx, secondReq); err != nil {
		t.Fatalf("reconcile ws-second: %v", err)
	}

	var second devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, secondReq.NamespacedName, &second); err != nil {
		t.Fatalf("get ws-second: %v", err)
	}
	if second.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Errorf("ws-second phase = %q, want mirrored Ready", second.Status.Phase)
	}
	if second.Status.WorkspaceId != "feature-dup" {
		t.Errorf("ws-second workspaceId = %q, want mirrored %q", second.Status.WorkspaceId, "feature-dup")
	}

	var stsList appsv1.StatefulSetList
	if err := testClient.List(ctx, &stsList, client.InNamespace(ns)); err != nil {
		t.Fatalf("list StatefulSets: %v", err)
	}
	if len(stsList.Items) != 1 {
		t.Errorf("expected exactly 1 StatefulSet in namespace, got %d", len(stsList.Items))
	}

	// 16.1/16.2: exactly one working directory (PVC) ever gets allocated for
	// the repository+branch pair, and ws-second never gets one of its own.
	var pvcList corev1.PersistentVolumeClaimList
	if err := testClient.List(ctx, &pvcList, client.InNamespace(ns)); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcList.Items) != 1 {
		t.Errorf("expected exactly 1 PVC in namespace, got %d", len(pvcList.Items))
	}
}

func TestReconcile_FailsAndRollsBackOnProvisioningTimeout(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	createWorkspace(t, ctx, ns, "ws-timeout", "https://gitea.fickledev.com/tom1022/demo.git", "feature/timeout", "default")

	r := newTestReconciler()
	r.ProvisioningTimeout = time.Nanosecond // already "elapsed" by the time Reconcile runs
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-timeout", Namespace: ns}}

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
	var cond *metav1.Condition
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == conditionProvisioned {
			cond = &ws.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Reason != "ProvisioningTimeout" {
		t.Fatalf("expected Provisioned condition with reason ProvisioningTimeout, got %+v", cond)
	}

	// Rollback: the StatefulSet (no protection finalizer) must be actually gone.
	// The PVC gets Kubernetes' built-in kubernetes.io/pvc-protection finalizer at
	// admission time; removing it is kube-controller-manager's job, which envtest
	// does not run, so here deletion only ever reaches "requested" (DeletionTimestamp
	// set) rather than "gone" — that is still sufficient evidence the rollback
	// issued the delete.
	resourceName := ws.Status.WorkspaceId
	err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		var sts appsv1.StatefulSet
		errSts := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &sts)
		if !apierrors.IsNotFound(errSts) {
			return false, nil
		}
		var pvc corev1.PersistentVolumeClaim
		errPvc := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc)
		if apierrors.IsNotFound(errPvc) {
			return true, nil
		}
		if errPvc == nil && !pvc.DeletionTimestamp.IsZero() {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("expected StatefulSet deleted and PVC deletion requested after rollback: %v", err)
	}
}
