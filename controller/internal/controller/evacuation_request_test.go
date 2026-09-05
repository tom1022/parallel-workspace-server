package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// fakeEvacuationRequester stands in for the Session Supervisor's /evacuate
// endpoint, recording who it was asked to evacuate.
type fakeEvacuationRequester struct {
	err   error
	calls []string
}

func (f *fakeEvacuationRequester) RequestEvacuation(_ context.Context, ws *devplatformv1alpha1.Workspace) (*devplatformv1alpha1.EvacuationSnapshot, error) {
	f.calls = append(f.calls, ws.Status.WorkspaceId)
	if f.err != nil {
		return nil, f.err
	}
	now := metav1.Now()
	return &devplatformv1alpha1.EvacuationSnapshot{
		WorkspaceId:     ws.Status.WorkspaceId,
		Branch:          ws.Spec.Branch,
		HeadCommit:      "0123456789abcdef0123456789abcdef01234567",
		BundleKey:       "workspace/" + ws.Status.WorkspaceId + "/latest/bundle.git",
		DirtyArchiveKey: "workspace/" + ws.Status.WorkspaceId + "/latest/dirty.tar.gz",
		CapturedAt:      &now,
		SizeBytes:       42,
	}, nil
}

type recordedNotification struct {
	workspace string
	kind      string
}

func recordingNotifier(into *[]recordedNotification) func(context.Context, *devplatformv1alpha1.Workspace, string, string) {
	return func(_ context.Context, ws *devplatformv1alpha1.Workspace, kind, _ string) {
		*into = append(*into, recordedNotification{workspace: ws.Name, kind: kind})
	}
}

// 16.8: suspension must not stop the execution environment until the working
// directory is off-node.
func TestReconcile_SuspendEvacuatesBeforeStoppingCompute(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evac-suspend", "feature/evac-suspend")
	req := types.NamespacedName{Name: "ws-evac-suspend", Namespace: ns}
	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))

	requester := &fakeEvacuationRequester{}
	r := newTestReconciler()
	r.EvacuationRequester = requester
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(requester.calls) != 1 || requester.calls[0] != resourceName {
		t.Fatalf("evacuation requests = %v, want one for %q", requester.calls, resourceName)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Errorf("phase = %q, want Suspended", ws.Status.Phase)
	}
	if ws.Status.LastEvacuation == nil {
		t.Fatal("status.lastEvacuation was not recorded")
	}
	if ws.Status.LastEvacuation.BundleKey == "" || ws.Status.LastEvacuation.DirtyArchiveKey == "" {
		t.Errorf("lastEvacuation = %+v, want both object keys", ws.Status.LastEvacuation)
	}
	if got := statefulSetReplicas(t, ctx, ns, resourceName); got != 0 {
		t.Errorf("StatefulSet replicas = %d, want 0", got)
	}
}

// design.md Evacuation Agent (Validation): an unavailable destination must not
// produce a stop without an evacuation.
func TestReconcile_SuspendStaysReadyWhenEvacuationFails(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evac-fail", "feature/evac-fail")
	req := types.NamespacedName{Name: "ws-evac-fail", Namespace: ns}
	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))

	var notes []recordedNotification
	r := newTestReconciler()
	r.EvacuationRequester = &fakeEvacuationRequester{err: errors.New("garage unreachable")}
	r.Notify = recordingNotifier(&notes)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Errorf("phase = %q, want Ready (suspension must not proceed unevacuated)", ws.Status.Phase)
	}
	if got := statefulSetReplicas(t, ctx, ns, resourceName); got != 1 {
		t.Errorf("StatefulSet replicas = %d, want 1 (execution environment must keep running)", got)
	}
	if ws.Status.LastEvacuation != nil {
		t.Error("a failed evacuation must not be recorded as a snapshot")
	}
	if len(notes) != 1 || notes[0].workspace != "ws-evac-fail" {
		t.Errorf("notifications = %+v, want one for ws-evac-fail", notes)
	}
}

// 16.4 + design.md Dependencies: destroy asks for an evacuation before the
// execution environment it would come from is torn down.
func TestReconcile_TerminatingEvacuatesBeforeDeletingCompute(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evac-destroy", "feature/evac-destroy")
	req := types.NamespacedName{Name: "ws-evac-destroy", Namespace: ns}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	requester := &fakeEvacuationRequester{}
	r := newTestReconciler()
	r.EvacuationRequester = requester
	r.EvacuationConfirmer = &StatusEvacuationConfirmer{}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(requester.calls) != 1 {
		t.Fatalf("evacuation requests = %v, want exactly one", requester.calls)
	}
	var sts appsv1.StatefulSet
	err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &sts)
	if err == nil && sts.DeletionTimestamp.IsZero() {
		t.Error("StatefulSet still present after a successful evacuation")
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get StatefulSet: %v", err)
	}
}

func TestReconcile_TerminatingKeepsComputeWhenEvacuationFails(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evac-destroy-fail", "feature/evac-destroy-fail")
	req := types.NamespacedName{Name: "ws-evac-destroy-fail", Namespace: ns}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	r := newTestReconciler()
	r.EvacuationRequester = &fakeEvacuationRequester{err: errors.New("garage unreachable")}
	r.EvacuationConfirmer = &StatusEvacuationConfirmer{}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while evacuation is still failing")
	}

	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &sts); err != nil {
		t.Fatalf("StatefulSet must survive a failed evacuation: %v", err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc); err != nil {
		t.Fatalf("PVC must survive a failed evacuation: %v", err)
	}
}

// Nothing is running to evacuate from once compute is stopped, so destroy must
// not stall waiting on a Pod that will never answer.
func TestReconcile_TerminatingSkipsEvacuationWithoutRunningCompute(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-evac-stopped", "feature/evac-stopped")
	req := types.NamespacedName{Name: "ws-evac-stopped", Namespace: ns}
	markStatefulSetNotReady(t, ctx, ns, resourceName)

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	requester := &fakeEvacuationRequester{err: errors.New("nothing is listening")}
	r := newTestReconciler()
	r.EvacuationRequester = requester
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(requester.calls) != 0 {
		t.Errorf("evacuation requested from stopped compute: %v", requester.calls)
	}
}

func TestStatusEvacuationConfirmer(t *testing.T) {
	confirmer := &StatusEvacuationConfirmer{}
	ws := &devplatformv1alpha1.Workspace{}

	complete, err := confirmer.IsEvacuationComplete(context.Background(), ws)
	if err != nil || complete {
		t.Errorf("no snapshot: complete=%v err=%v", complete, err)
	}

	ws.Status.LastEvacuation = &devplatformv1alpha1.EvacuationSnapshot{BundleKey: "workspace/ws/latest/bundle.git"}
	complete, err = confirmer.IsEvacuationComplete(context.Background(), ws)
	if err != nil || !complete {
		t.Errorf("with snapshot: complete=%v err=%v", complete, err)
	}
}

func TestSupervisorEvacuationRequesterPostsToWorkspacePod(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspaceId":"ws-1","branch":"feature/x","headCommit":"abc",` +
			`"bundleKey":"workspace/ws-1/latest/bundle.git","dirtyArchiveKey":"workspace/ws-1/latest/dirty.tar.gz",` +
			`"capturedAt":"2026-09-05T12:00:00Z","sizeBytes":128}`))
	}))
	defer srv.Close()
	host, portStr, err := splitHostPort(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	ws := &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: ns},
		Spec:       devplatformv1alpha1.WorkspaceSpec{Repository: "https://example.com/r.git", Branch: "feature/x", TemplateRef: "default"},
	}
	ws.Status.WorkspaceId = "ws-1"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1-0", Namespace: ns, Labels: workspaceLabels(ws)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: "busybox"}}},
	}
	if err := testClient.Create(ctx, pod); err != nil {
		t.Fatalf("create Pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = host
	if err := testClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("set Pod status: %v", err)
	}

	requester := &SupervisorEvacuationRequester{Client: testClient, Port: port}
	snap, err := requester.RequestEvacuation(ctx, ws)
	if err != nil {
		t.Fatalf("RequestEvacuation: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/evacuate" {
		t.Errorf("request = %s %s, want POST /evacuate", gotMethod, gotPath)
	}
	if snap.BundleKey != "workspace/ws-1/latest/bundle.git" || snap.SizeBytes != 128 {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.CapturedAt == nil {
		t.Error("capturedAt was not decoded")
	}
}

func TestSupervisorEvacuationRequesterFailsWithoutARunningPod(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := &devplatformv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-none", Namespace: ns}}
	ws.Status.WorkspaceId = "ws-none"

	if _, err := (&SupervisorEvacuationRequester{Client: testClient}).RequestEvacuation(ctx, ws); err == nil {
		t.Fatal("expected an error when no workspace Pod is running")
	}
}

func splitHostPort(rawURL string) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), u.Port(), nil
}

func TestHermesNotifierPostsTheEvent(t *testing.T) {
	got := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
	}))
	defer srv.Close()

	notify := HermesNotifier(srv.URL)
	if notify == nil {
		t.Fatal("a configured endpoint must yield a notifier")
	}
	notify(context.Background(), &devplatformv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-1"}}, EventEvacuationFailed, "garage unreachable")

	select {
	case body := <-got:
		if body["kind"] != EventEvacuationFailed || body["workspace"] != "ws-1" || body["detail"] != "garage unreachable" {
			t.Errorf("event = %v", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification was delivered")
	}

	if HermesNotifier("") != nil {
		t.Error("an unset endpoint must not produce a notifier")
	}
}

// The snapshot has to reach the API server, not just the in-memory object:
// once the StatefulSet is gone there is nothing left to re-evacuate from, so a
// retry that re-read the Workspace would find no record and stall on the
// finalizer forever.
func TestReconcile_TerminatingPersistsTheSnapshotBeforeTearingDownCompute(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReady(t, ctx, ns, "ws-evac-persist", "feature/evac-persist")
	req := types.NamespacedName{Name: "ws-evac-persist", Namespace: ns}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	r := newTestReconciler()
	r.EvacuationRequester = &fakeEvacuationRequester{}
	// Reported incomplete so the finalizer holds the object around long enough
	// to read its persisted status back.
	r.EvacuationConfirmer = &fakeEvacuationConfirmer{complete: false}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var stored devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &stored); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if stored.Status.LastEvacuation == nil {
		t.Fatal("status.lastEvacuation was not persisted before compute was torn down")
	}
	if stored.Status.LastEvacuation.BundleKey == "" {
		t.Errorf("lastEvacuation = %+v", stored.Status.LastEvacuation)
	}
}
