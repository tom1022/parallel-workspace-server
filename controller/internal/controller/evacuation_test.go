package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// fakeEvacuationConfirmer is the test double for the real Garage-backed
// EvacuationConfirmer (task 4). It also records every workspace id it was
// asked about, so a test can assert the reconciler queried the right one.
type fakeEvacuationConfirmer struct {
	complete bool
	err      error

	calls []string
}

func (f *fakeEvacuationConfirmer) IsEvacuationComplete(_ context.Context, ws *devplatformv1alpha1.Workspace) (bool, error) {
	f.calls = append(f.calls, ws.Status.WorkspaceId)
	return f.complete, f.err
}

// TestReconcile_TerminatingDeletesPVCAndFinalizerOnceEvacuationConfirmed
// covers the other half of 16.4 from lifecycle_test.go's
// TestReconcile_TerminatingDeletesSubstrateAndSetsPhase: once
// EvacuationConfirmer reports completion, the PVC delete request is actually
// issued and evacuationFinalizer is released, letting the Workspace object
// itself disappear (no other finalizer holds it in this test).
func TestReconcile_TerminatingDeletesPVCAndFinalizerOnceEvacuationConfirmed(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evacuated", "feature/evacuated")
	req := types.NamespacedName{Name: "ws-evacuated", Namespace: ns}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	confirmer := &fakeEvacuationConfirmer{complete: true}
	r := newTestReconciler()
	r.EvacuationConfirmer = confirmer
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile (terminating): %v", err)
	}

	if len(confirmer.calls) != 1 || confirmer.calls[0] != resourceName {
		t.Errorf("expected EvacuationConfirmer queried once for %q, got calls=%v", resourceName, confirmer.calls)
	}

	var pvc corev1.PersistentVolumeClaim
	errPvc := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc)
	if !(apierrors.IsNotFound(errPvc) || (errPvc == nil && !pvc.DeletionTimestamp.IsZero())) {
		t.Errorf("expected PVC deleted or deletion-requested once evacuation is confirmed, err=%v deletionTimestamp=%v", errPvc, pvc.DeletionTimestamp)
	}

	if err := testClient.Get(ctx, req, &ws); !apierrors.IsNotFound(err) {
		t.Errorf("expected Workspace itself gone once evacuationFinalizer is released, err=%v", err)
	}
}

// TestReconcile_TerminatingDuplicateWorkspaceDoesNotDeleteCanonicalSubstrate
// guards 16.1/16.2 on the destroy path: a Workspace that only ever mirrored
// another canonical workspace's substrate (1.6) must not reach into that
// still-live substrate when it is itself destroyed, even though its own
// status.workspaceId is the canonical's id.
func TestReconcile_TerminatingDuplicateWorkspaceDoesNotDeleteCanonicalSubstrate(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	repo := "https://gitea.fickledev.com/tom1022/demo.git"
	branch := "feature/dup-terminate"

	createWorkspace(t, ctx, ns, "ws-canonical", repo, branch, "default")
	r := newTestReconciler()
	canonicalReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-canonical", Namespace: ns}}
	if _, err := r.Reconcile(ctx, canonicalReq); err != nil {
		t.Fatalf("reconcile ws-canonical: %v", err)
	}
	markStatefulSetReady(t, ctx, ns, "feature-dup-terminate")
	if _, err := r.Reconcile(ctx, canonicalReq); err != nil {
		t.Fatalf("reconcile ws-canonical (ready pass): %v", err)
	}

	createWorkspace(t, ctx, ns, "ws-mirror", repo, branch, "default")
	mirrorReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-mirror", Namespace: ns}}
	if _, err := r.Reconcile(ctx, mirrorReq); err != nil {
		t.Fatalf("reconcile ws-mirror: %v", err)
	}

	var mirror devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, mirrorReq.NamespacedName, &mirror); err != nil {
		t.Fatalf("get ws-mirror: %v", err)
	}
	if err := testClient.Delete(ctx, &mirror); err != nil {
		t.Fatalf("delete ws-mirror: %v", err)
	}
	if _, err := r.Reconcile(ctx, mirrorReq); err != nil {
		t.Fatalf("reconcile ws-mirror (terminating): %v", err)
	}

	if err := testClient.Get(ctx, mirrorReq.NamespacedName, &mirror); !apierrors.IsNotFound(err) {
		t.Errorf("expected ws-mirror itself gone, err=%v", err)
	}

	resourceName := "feature-dup-terminate"
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc); err != nil {
		t.Fatalf("expected canonical's PVC to survive ws-mirror's destroy, err=%v", err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		t.Error("expected canonical's PVC to have no deletion request after ws-mirror's destroy")
	}

	var canonical devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, canonicalReq.NamespacedName, &canonical); err != nil {
		t.Fatalf("get ws-canonical after ws-mirror's destroy: %v", err)
	}
	if canonical.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Errorf("expected ws-canonical to remain Ready, got %q", canonical.Status.Phase)
	}
}

// TestBuildStatefulSet_InitScriptClearsStaleGitLocks covers 16.5: a crash
// mid-clone/checkout can leave a git lock file behind, and this initContainer
// script is what a resumed/restarted Pod runs before its own git operations
// would otherwise fail against it.
func TestBuildStatefulSet_InitScriptClearsStaleGitLocks(t *testing.T) {
	ws := &devplatformv1alpha1.Workspace{
		Spec: devplatformv1alpha1.WorkspaceSpec{
			Repository: "https://gitea.fickledev.com/tom1022/demo.git",
			Branch:     "feature/lock-test",
		},
	}
	tmpl := &devplatformv1alpha1.WorkspaceTemplate{
		Spec: devplatformv1alpha1.WorkspaceTemplateSpec{
			Image:    "busybox:1.36",
			NodeName: "test-node",
			Resources: devplatformv1alpha1.WorkspaceResources{
				Requests: devplatformv1alpha1.ResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   devplatformv1alpha1.ResourceList{CPU: "500m", Memory: "256Mi"},
			},
			Auth: devplatformv1alpha1.WorkspaceAuthRef{SecretRef: "claude-auth"},
		},
	}

	sts, err := buildStatefulSet(ws, tmpl, "feature-lock-test")
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}
	if got := sts.Spec.Template.Spec.InitContainers[0].Name; got != "workspace-init" {
		t.Fatalf("first init container = %q, want the checkout", got)
	}
	cmd := sts.Spec.Template.Spec.InitContainers[0].Command
	script := cmd[len(cmd)-1]
	for _, lock := range []string{"index.lock", "HEAD.lock"} {
		if !strings.Contains(script, lock) {
			t.Errorf("init script missing stale lock cleanup for %q:\n%s", lock, script)
		}
	}
}
