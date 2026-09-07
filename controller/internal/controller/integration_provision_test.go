package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/database"
)

// provisionIntSubstrate is the full inventory of incidental resources a
// workspace can own, so each lifecycle stage asserts on what must be absent
// as well as what must be present.
type provisionIntSubstrate struct {
	pvc           bool
	statefulSet   bool
	database      bool
	databaseRole  bool
	claudeMD      bool
	gitCredential bool
	previewRoute  bool
	reportRoute   bool
}

// provisionIntLive treats a resource that only carries a deletion request as
// absent: envtest runs no kube-controller-manager, so a PVC held by the
// built-in pvc-protection finalizer never actually disappears.
func provisionIntLive(t *testing.T, ctx context.Context, ns, name string, obj client.Object) bool {
	t.Helper()
	err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get %T %s: %v", obj, name, err)
	}
	return obj.GetDeletionTimestamp().IsZero()
}

func provisionIntObserve(t *testing.T, ctx context.Context, ns, resourceName string) provisionIntSubstrate {
	t.Helper()
	return provisionIntSubstrate{
		pvc:           provisionIntLive(t, ctx, ns, resourceName, &corev1.PersistentVolumeClaim{}),
		statefulSet:   provisionIntLive(t, ctx, ns, resourceName, &appsv1.StatefulSet{}),
		database:      provisionIntLive(t, ctx, ns, resourceName, database.CNPGDatabaseRef(ns, resourceName)),
		databaseRole:  provisionIntLive(t, ctx, ns, resourceName, database.CNPGDatabaseRoleRef(ns, resourceName)),
		claudeMD:      provisionIntLive(t, ctx, ns, ClaudeMDConfigMapName(resourceName), &corev1.ConfigMap{}),
		gitCredential: provisionIntLive(t, ctx, ns, resourceName, &corev1.Secret{}),
		previewRoute:  provisionIntLive(t, ctx, ns, resourceName+hostSuffixPreview, ingressRouteRef(ns, resourceName+hostSuffixPreview)),
		reportRoute:   provisionIntLive(t, ctx, ns, resourceName+hostSuffixReport, ingressRouteRef(ns, resourceName+hostSuffixReport)),
	}
}

func provisionIntAssertSubstrate(t *testing.T, ctx context.Context, ns, resourceName, stage string, want provisionIntSubstrate) {
	t.Helper()
	if got := provisionIntObserve(t, ctx, ns, resourceName); got != want {
		t.Errorf("%s: substrate = %+v, want %+v", stage, got, want)
	}
}

func provisionIntGet(t *testing.T, ctx context.Context, req types.NamespacedName) *devplatformv1alpha1.Workspace {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	return &ws
}

// TestIntegration_ProvisionRunSuspendResumeDestroy walks one workspace through
// every lifecycle stage in a single run, asserting after each stage that
// exactly the expected incidental resources exist (1.8, 8.10, 13.2, 13.3,
// 13.7). The per-stage suites assert each transition in isolation; this one
// exists for the ordering between them — evacuation before compute stops, the
// database surviving suspension, and routing outliving the database teardown
// until the workspace object itself is released.
func TestIntegration_ProvisionRunSuspendResumeDestroy(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	createWorkspace(t, ctx, ns, "ws-e2e", "https://gitea.fickledev.com/tom1022/demo.git", "feature/e2e", "default")
	req := types.NamespacedName{Name: "ws-e2e", Namespace: ns}

	evacuator := &fakeEvacuationRequester{}
	confirmer := &fakeEvacuationConfirmer{}
	r := newTestReconciler()
	r.EvacuationRequester = evacuator
	r.EvacuationConfirmer = confirmer
	request := ctrl.Request{NamespacedName: req}

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (provisioning): %v", err)
	}
	ws := provisionIntGet(t, ctx, req)
	resourceName := ws.Status.WorkspaceId
	if resourceName == "" {
		t.Fatal("workspaceId is empty after the first reconcile")
	}
	// Routing and the git credential are deliberately not yet issued: the pod
	// they point at is not running.
	provisionIntAssertSubstrate(t, ctx, ns, resourceName, "provisioning", provisionIntSubstrate{
		pvc: true, statefulSet: true, database: true, databaseRole: true, claudeMD: true,
	})

	markStatefulSetReady(t, ctx, ns, resourceName)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (session start): %v", err)
	}
	ws = provisionIntGet(t, ctx, req)
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready", ws.Status.Phase)
	}
	if ws.Status.SessionId == "" || ws.Status.Urls.Session == "" {
		t.Errorf("Ready workspace must carry a session id and connection URL, got %+v", ws.Status)
	}
	provisionIntAssertSubstrate(t, ctx, ns, resourceName, "ready", provisionIntSubstrate{
		pvc: true, statefulSet: true, database: true, databaseRole: true, claudeMD: true,
		gitCredential: true, previewRoute: true, reportRoute: true,
	})
	var readyPVC corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &readyPVC); err != nil {
		t.Fatalf("get PVC: %v", err)
	}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (suspend): %v", err)
	}
	ws = provisionIntGet(t, ctx, req)
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Fatalf("phase = %q, want Suspended", ws.Status.Phase)
	}
	if len(evacuator.calls) != 1 || evacuator.calls[0] != resourceName {
		t.Errorf("evacuation requests = %v, want exactly one for %q before compute stops", evacuator.calls, resourceName)
	}
	if ws.Status.LastEvacuation == nil {
		t.Error("suspended workspace has no evacuation snapshot recorded")
	}
	if got := statefulSetReplicas(t, ctx, ns, resourceName); got != 0 {
		t.Errorf("StatefulSet replicas = %d, want 0 after suspend", got)
	}
	// 8.10 / 13.2: suspension stops compute only. Database, working directory
	// and routing all survive so a resume reconnects to the same substrate.
	provisionIntAssertSubstrate(t, ctx, ns, resourceName, "suspended", provisionIntSubstrate{
		pvc: true, statefulSet: true, database: true, databaseRole: true, claudeMD: true,
		gitCredential: true, previewRoute: true, reportRoute: true,
	})

	markStatefulSetNotReady(t, ctx, ns, resourceName)
	if err := r.MarkActivity(ctx, ws); err != nil {
		t.Fatalf("MarkActivity: %v", err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (resume, pod not yet ready): %v", err)
	}
	if got := provisionIntGet(t, ctx, req).Status.Phase; got != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Errorf("phase = %q while the resumed pod is not ready, want Suspended", got)
	}
	markStatefulSetReady(t, ctx, ns, resourceName)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (resume): %v", err)
	}
	ws = provisionIntGet(t, ctx, req)
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready after resume", ws.Status.Phase)
	}
	if ws.Status.SessionId == "" {
		t.Error("resumed workspace has no session id")
	}
	var resumedPVC corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &resumedPVC); err != nil {
		t.Fatalf("get PVC after resume: %v", err)
	}
	if resumedPVC.UID != readyPVC.UID {
		t.Errorf("PVC UID changed across suspend/resume (%q -> %q); the retained working directory must be reused", readyPVC.UID, resumedPVC.UID)
	}

	confirmer.complete = false
	if err := testClient.Delete(ctx, ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (terminating, evacuation unconfirmed): %v", err)
	}
	if got := provisionIntGet(t, ctx, req).Status.Phase; got != devplatformv1alpha1.WorkspacePhaseTerminating {
		t.Fatalf("phase = %q, want Terminating", got)
	}
	if len(evacuator.calls) != 2 {
		t.Errorf("evacuation requests = %v, want a second one before compute is torn down", evacuator.calls)
	}
	// Routing outlives the database teardown: it is removed by the garbage
	// collector once the workspace object itself goes, which the evacuation
	// gate is still holding back.
	provisionIntAssertSubstrate(t, ctx, ns, resourceName, "terminating", provisionIntSubstrate{
		pvc: true, claudeMD: true, gitCredential: true, previewRoute: true, reportRoute: true,
	})

	confirmer.complete = true
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (terminating, evacuation confirmed): %v", err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc); err != nil {
		t.Fatalf("get PVC after destroy: %v", err)
	}
	if pvc.DeletionTimestamp.IsZero() {
		t.Error("working directory was not deleted once evacuation was confirmed (13.7)")
	}
	if err := testClient.Get(ctx, req, &devplatformv1alpha1.Workspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("get Workspace after destroy: err = %v, want NotFound (finalizer released)", err)
	}

	// What the garbage collector removes on that release, rather than the
	// controller, still has to be reachable from the deleted object.
	for name, obj := range map[string]client.Object{
		ClaudeMDConfigMapName(resourceName): &corev1.ConfigMap{},
		resourceName:                        &corev1.Secret{},
		resourceName + hostSuffixPreview:    ingressRouteRef(ns, resourceName+hostSuffixPreview),
		resourceName + hostSuffixReport:     ingressRouteRef(ns, resourceName+hostSuffixReport),
	} {
		if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			t.Fatalf("get %s: %v", name, err)
		}
		owners := obj.GetOwnerReferences()
		if len(owners) != 1 || owners[0].UID != ws.UID || owners[0].Controller == nil || !*owners[0].Controller {
			t.Errorf("%s owner references = %+v, want a single controller reference to Workspace %q", name, owners, ws.UID)
		}
	}
}

// TestIntegration_FailedProvisioningLeavesNoSubstrate covers 1.8 for a failure
// that happens after routing and the ClaudeMD document already exist: the
// workspace object survives in Failed, so nothing gets garbage collected and
// the rollback itself has to remove every partially created resource.
func TestIntegration_FailedProvisioningLeavesNoSubstrate(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	createWorkspace(t, ctx, ns, "ws-rollback", "https://gitea.fickledev.com/tom1022/demo.git", "feature/rollback", "default")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-rollback", Namespace: ns}}

	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (provisioning): %v", err)
	}
	ws := provisionIntGet(t, ctx, req.NamespacedName)
	resourceName := ws.Status.WorkspaceId

	markStatefulSetReady(t, ctx, ns, resourceName)
	r.GitCredentialIssuer = &fakeGitCredentialIssuer{err: errors.New("git hosting unavailable")}
	r.ProvisioningTimeout = time.Nanosecond
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (failing pass): %v", err)
	}

	ws = provisionIntGet(t, ctx, req.NamespacedName)
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase = %q, want Failed", ws.Status.Phase)
	}
	provisionIntAssertSubstrate(t, ctx, ns, resourceName, "failed", provisionIntSubstrate{})
}
