package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// evacIntSupervisor stands in for the workspace Pod's supervisor endpoint,
// serving the same contract cmd/supervisor's POST /evacuate does: a snapshot
// once the working directory is off-node, HTTP 500 while the destination
// cannot be written to. Unlike the fakes the per-stage suites inject at the
// EvacuationRequester seam, this drives the real SupervisorEvacuationRequester
// over HTTP, so the destination being unavailable is expressed the way it
// actually reaches the controller.
type evacIntSupervisor struct {
	host string

	mu            sync.Mutex
	destinationUp bool
	calls         int
}

func evacIntStartSupervisor(t *testing.T) (*evacIntSupervisor, int) {
	t.Helper()
	s := &evacIntSupervisor{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls++
		if !s.destinationUp {
			http.Error(w, "evacuation: object store unreachable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspaceId":     "evac-int",
			"branch":          "feature/evac-int",
			"headCommit":      "0123456789abcdef0123456789abcdef01234567",
			"bundleKey":       "workspace/evac-int/latest/bundle.git",
			"dirtyArchiveKey": "workspace/evac-int/latest/dirty.tar.gz",
			"capturedAt":      time.Now().UTC().Format(time.RFC3339),
			"sizeBytes":       4096,
		})
	}))
	t.Cleanup(srv.Close)

	host, portStr, err := splitHostPort(srv.URL)
	if err != nil {
		t.Fatalf("parse supervisor URL: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse supervisor port: %v", err)
	}
	s.host = host
	return s, port
}

func (s *evacIntSupervisor) setDestinationUp(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destinationUp = up
}

func (s *evacIntSupervisor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// evacIntRunningPod publishes the address the controller resolves a workspace's
// supervisor by. envtest runs no kubelet, so the Pod a StatefulSet would create
// and the IP a CNI would assign are both written here by hand.
func evacIntRunningPod(t *testing.T, ctx context.Context, ns, wsName, podIP string) {
	t.Helper()
	ws := &devplatformv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: wsName, Namespace: ns}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: wsName + "-0", Namespace: ns, Labels: workspaceLabels(ws)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: "busybox:1.36"}}},
	}
	if err := testClient.Create(ctx, pod); err != nil {
		t.Fatalf("create workspace Pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = podIP
	if err := testClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("set workspace Pod status: %v", err)
	}
}

func evacIntReconciler(t *testing.T, sup *evacIntSupervisor, port int, notes *[]recordedNotification) *WorkspaceReconciler {
	t.Helper()
	r := newTestReconciler()
	r.EvacuationRequester = &SupervisorEvacuationRequester{Client: testClient, Port: port}
	r.EvacuationConfirmer = StatusEvacuationConfirmer{}
	r.Notify = recordingNotifier(notes)
	return r
}

func evacIntWorkspace(t *testing.T, ctx context.Context, req types.NamespacedName) *devplatformv1alpha1.Workspace {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	return &ws
}

func evacIntPVCLive(t *testing.T, ctx context.Context, ns, name string) bool {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pvc)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get PVC %s: %v", name, err)
	}
	return pvc.DeletionTimestamp.IsZero()
}

// TestIntegration_SuspendWaitsForAnAvailableEvacuationDestination drives one
// workspace across the whole 16.8 boundary with the real HTTP requester: while
// the destination cannot be written to there is no stop at all, and the stop
// that eventually happens carries a snapshot.
func TestIntegration_SuspendWaitsForAnAvailableEvacuationDestination(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evac-int-stop", "feature/evac-int-stop")
	req := types.NamespacedName{Name: "ws-evac-int-stop", Namespace: ns}

	sup, port := evacIntStartSupervisor(t)
	evacIntRunningPod(t, ctx, ns, "ws-evac-int-stop", sup.host)
	var notes []recordedNotification
	r := evacIntReconciler(t, sup, port, &notes)
	request := ctrl.Request{NamespacedName: req}

	// An explicit suspend request, so the refusal below is unambiguously the
	// evacuation gate rather than the idle clock not having run out.
	ws := evacIntWorkspace(t, ctx, req)
	ws.Spec.DesiredPhase = devplatformv1alpha1.DesiredPhaseSuspended
	if err := testClient.Update(ctx, ws); err != nil {
		t.Fatalf("request suspension: %v", err)
	}

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (destination down): %v", err)
	}
	if sup.callCount() == 0 {
		t.Error("the supervisor was never asked to evacuate")
	}
	ws = evacIntWorkspace(t, ctx, req)
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Errorf("phase = %q, want Ready while the destination is unavailable", ws.Status.Phase)
	}
	if got := statefulSetReplicas(t, ctx, ns, resourceName); got != 1 {
		t.Errorf("StatefulSet replicas = %d, want 1 (compute must keep running)", got)
	}
	if ws.Status.LastEvacuation != nil {
		t.Error("a refused evacuation must not be recorded as a snapshot")
	}
	if !meta.IsStatusConditionFalse(ws.Status.Conditions, conditionEvacuated) {
		t.Errorf("Evacuated condition = %+v, want False", meta.FindStatusCondition(ws.Status.Conditions, conditionEvacuated))
	}
	if len(notes) != 1 || notes[0].kind != EventEvacuationFailed {
		t.Errorf("notifications = %+v, want one %s", notes, EventEvacuationFailed)
	}

	sup.setDestinationUp(true)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (destination back): %v", err)
	}
	ws = evacIntWorkspace(t, ctx, req)
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Errorf("phase = %q, want Suspended once the working directory is off-node", ws.Status.Phase)
	}
	if ws.Status.LastEvacuation == nil || ws.Status.LastEvacuation.BundleKey == "" {
		t.Fatalf("lastEvacuation = %+v, want the supervisor's snapshot", ws.Status.LastEvacuation)
	}
	if got := statefulSetReplicas(t, ctx, ns, resourceName); got != 0 {
		t.Errorf("StatefulSet replicas = %d, want 0 after the evacuated stop", got)
	}
	if !evacIntPVCLive(t, ctx, ns, resourceName) {
		t.Error("the working directory must be retained across suspension")
	}
}

// TestIntegration_DestroyKeepsTheVolumeUntilEvacuationCompletes is 16.4's
// counterpart: the destroy request is accepted immediately, but nothing that
// holds the working directory is released until the evacuation behind it has
// actually succeeded.
func TestIntegration_DestroyKeepsTheVolumeUntilEvacuationCompletes(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evac-int-destroy", "feature/evac-int-destroy")
	req := types.NamespacedName{Name: "ws-evac-int-destroy", Namespace: ns}

	sup, port := evacIntStartSupervisor(t)
	evacIntRunningPod(t, ctx, ns, "ws-evac-int-destroy", sup.host)
	var notes []recordedNotification
	r := evacIntReconciler(t, sup, port, &notes)
	request := ctrl.Request{NamespacedName: req}

	if err := testClient.Delete(ctx, evacIntWorkspace(t, ctx, req)); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	res, err := r.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("reconcile (destination down): %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while the evacuation behind the destroy is still failing")
	}
	ws := evacIntWorkspace(t, ctx, req)
	if !controllerutil.ContainsFinalizer(ws, evacuationFinalizer) {
		t.Error("the evacuation-wait finalizer was released before any evacuation succeeded")
	}
	if ws.Status.LastEvacuation != nil {
		t.Error("a refused evacuation must not be recorded as a snapshot")
	}
	if !evacIntPVCLive(t, ctx, ns, resourceName) {
		t.Error("the working directory was released before the evacuation completed")
	}
	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &sts); err != nil {
		t.Fatalf("compute must survive a failed evacuation: %v", err)
	}

	sup.setDestinationUp(true)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile (destination back): %v", err)
	}
	if evacIntPVCLive(t, ctx, ns, resourceName) {
		t.Error("the working directory was not released once the evacuation completed")
	}
	if err := testClient.Get(ctx, req, &devplatformv1alpha1.Workspace{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected the Workspace released once evacuation is confirmed, err=%v", err)
	}
}
