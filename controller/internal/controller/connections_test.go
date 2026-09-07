package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// fakeSSHSessionCounter stands in for the Session Supervisor's published SSH
// session count.
type fakeSSHSessionCounter struct {
	count int
	err   error
}

func (f *fakeSSHSessionCounter) SSHSessionCount(context.Context, *devplatformv1alpha1.Workspace) (int, error) {
	return f.count, f.err
}

// setBrowserConnections is the Terminal Gateway's connect/disconnect
// notification as the controller sees it: an annotation on the Workspace, not
// a status write.
func setBrowserConnections(t *testing.T, ctx context.Context, req types.NamespacedName, n int) {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	annotations := ws.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if n == 0 {
		delete(annotations, AnnotationBrowserConnections)
	} else {
		annotations[AnnotationBrowserConnections] = strconv.Itoa(n)
	}
	ws.SetAnnotations(annotations)
	if err := testClient.Update(ctx, &ws); err != nil {
		t.Fatalf("set browser connections: %v", err)
	}
}

func phaseOf(t *testing.T, ctx context.Context, req types.NamespacedName) devplatformv1alpha1.WorkspacePhase {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	return ws.Status.Phase
}

// 4.8: a browser connection counts as a connection, so the idle window never
// runs out underneath a developer who is watching the session.
func TestReconcile_DoesNotSuspendWhileBrowserConnected(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReady(t, ctx, ns, "ws-browser", "feature/browser")
	req := types.NamespacedName{Name: "ws-browser", Namespace: ns}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))
	setBrowserConnections(t, ctx, req, 1)

	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := phaseOf(t, ctx, req); got != devplatformv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready while a browser is connected", got)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatal(err)
	}
	if ws.Status.LastActivityAt == nil || time.Since(ws.Status.LastActivityAt.Time) > time.Minute {
		t.Fatalf("lastActivityAt = %v; the idle clock must not advance while connected", ws.Status.LastActivityAt)
	}
}

// 4.8: an SSH session alone, with no browser connection at all, is still a
// connection.
func TestReconcile_DoesNotSuspendWhileOnlySSHSessionIsOpen(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReady(t, ctx, ns, "ws-ssh", "feature/ssh")
	req := types.NamespacedName{Name: "ws-ssh", Namespace: ns}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))

	r := newTestReconciler()
	r.SSHSessionCounter = &fakeSSHSessionCounter{count: 1}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if got := phaseOf(t, ctx, req); got != devplatformv1alpha1.WorkspacePhaseReady {
			t.Fatalf("phase = %q after reconcile %d, want Ready while an SSH session is open", got, i)
		}
	}
}

// 4.8: the elapsed time only starts running at disconnect.
func TestReconcile_SuspendsOnlyAfterDisconnect(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReady(t, ctx, ns, "ws-disconnect", "feature/disconnect")
	req := types.NamespacedName{Name: "ws-disconnect", Namespace: ns}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))
	setBrowserConnections(t, ctx, req, 1)

	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile (connected): %v", err)
	}

	setBrowserConnections(t, ctx, req, 0)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile (just disconnected): %v", err)
	}
	if got := phaseOf(t, ctx, req); got != devplatformv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q right after disconnect, want Ready: the idle window restarts at disconnect", got)
	}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile (idle after disconnect): %v", err)
	}
	if got := phaseOf(t, ctx, req); got != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Fatalf("phase = %q, want Suspended once the idle window elapsed after disconnect", got)
	}
}

// An explicit suspend request is an operator decision, not idle detection, so
// a connection must not veto it.
func TestReconcile_ExplicitSuspendRequestOutranksConnections(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReady(t, ctx, ns, "ws-forced", "feature/forced")
	req := types.NamespacedName{Name: "ws-forced", Namespace: ns}
	setBrowserConnections(t, ctx, req, 1)

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatal(err)
	}
	ws.Spec.DesiredPhase = devplatformv1alpha1.DesiredPhaseSuspended
	if err := testClient.Update(ctx, &ws); err != nil {
		t.Fatal(err)
	}

	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := phaseOf(t, ctx, req); got != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Fatalf("phase = %q, want Suspended", got)
	}
}

func TestSupervisorSSHSessionCounterReadsThePublishedCount(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"count":3}`))
	}))
	defer srv.Close()
	host, portStr, err := splitHostPort(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	ws := &devplatformv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-count", Namespace: ns}}
	ws.Status.WorkspaceId = "ws-count"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-count-0", Namespace: ns, Labels: workspaceLabels(ws)},
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

	count, err := (&SupervisorSSHSessionCounter{Client: testClient, Port: port}).SSHSessionCount(ctx, ws)
	if err != nil {
		t.Fatalf("SSHSessionCount: %v", err)
	}
	if gotPath != "/ssh-sessions" {
		t.Errorf("path = %q, want /ssh-sessions", gotPath)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

// A workspace whose supervisor cannot be reached reads as having no sessions:
// suspending it is still gated on the evacuation succeeding, so the failure
// mode is a stopped workspace, never lost work.
func TestSupervisorSSHSessionCounterReportsNoSessionsWithoutARunningPod(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	ws := &devplatformv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-nopod", Namespace: ns}}
	ws.Status.WorkspaceId = "ws-nopod"

	count, err := (&SupervisorSSHSessionCounter{Client: testClient}).SSHSessionCount(ctx, ws)
	if err != nil {
		t.Fatalf("SSHSessionCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}
