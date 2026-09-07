package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// fakeChangedFilesReporter stands in for the Session Supervisor's published
// changed-file list.
type fakeChangedFilesReporter struct {
	files []string
	err   error
}

func (f *fakeChangedFilesReporter) ChangedFiles(context.Context, *devplatformv1alpha1.Workspace) ([]string, error) {
	return f.files, f.err
}

// provisionToReadyWithSummary is provisionToReady with the work summary and
// public interfaces the requester supplies present from creation, which is
// what 10.2 asks the entry to be registered from.
func provisionToReadyWithSummary(t *testing.T, ctx context.Context, ns, name, branch, summary, interfaces string) {
	t.Helper()
	ws := createWorkspace(t, ctx, ns, name, "https://gitea.fickledev.com/tom1022/demo.git", branch, "default")
	ws.SetAnnotations(map[string]string{
		AnnotationBlackboardSummary:          summary,
		AnnotationBlackboardPublicInterfaces: interfaces,
	})
	if err := testClient.Update(ctx, ws); err != nil {
		t.Fatalf("annotate Workspace: %v", err)
	}

	r := newTestReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var current devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req.NamespacedName, &current); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	markStatefulSetReady(t, ctx, ns, current.Status.WorkspaceId)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
}

func blackboardOf(t *testing.T, ctx context.Context, req types.NamespacedName) *devplatformv1alpha1.BlackboardEntry {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	return ws.Status.Blackboard
}

// 10.1/10.2: provisioning a workspace registers its branch's entry with the
// work summary and public interfaces the requester stated.
func TestReconcile_CreatesBlackboardEntryOnProvision(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-create", "feature/bb-create", "認証基盤の刷新", "AuthService.Login,AuthService.Logout")
	req := types.NamespacedName{Name: "ws-bb-create", Namespace: ns}

	entry := blackboardOf(t, ctx, req)
	if entry == nil {
		t.Fatal("status.blackboard is nil, want an entry created at provisioning")
	}
	if entry.Branch != "feature/bb-create" {
		t.Errorf("branch = %q, want feature/bb-create", entry.Branch)
	}
	if entry.Summary != "認証基盤の刷新" {
		t.Errorf("summary = %q, want 認証基盤の刷新", entry.Summary)
	}
	if !slices.Equal(entry.PublicInterfaces, []string{"AuthService.Login", "AuthService.Logout"}) {
		t.Errorf("publicInterfaces = %v, want the two annotated names", entry.PublicInterfaces)
	}
	if !entry.Active {
		t.Error("active = false, want an entry active while the workspace exists")
	}
	if entry.UpdatedAt == nil {
		t.Error("updatedAt is nil, want it stamped when the entry is written")
	}
}

// 10.3: the changed-file list follows what the agent has actually touched.
func TestReconcile_UpdatesChangedFilesFromSupervisor(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-files", "feature/bb-files", "認証基盤の刷新", "")
	req := types.NamespacedName{Name: "ws-bb-files", Namespace: ns}

	r := newTestReconciler()
	r.ChangedFilesReporter = &fakeChangedFilesReporter{files: []string{"internal/auth/login.go", "internal/auth/logout.go"}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	entry := blackboardOf(t, ctx, req)
	if entry == nil {
		t.Fatal("status.blackboard is nil")
	}
	if !slices.Equal(entry.ChangedFiles, []string{"internal/auth/login.go", "internal/auth/logout.go"}) {
		t.Fatalf("changedFiles = %v, want the two files the supervisor reported", entry.ChangedFiles)
	}
}

// An unreachable supervisor must not erase what the branch is known to be
// changing; a stale list is more useful to the other branches than none.
func TestReconcile_KeepsChangedFilesWhenSupervisorUnreachable(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-stale", "feature/bb-stale", "認証基盤の刷新", "")
	req := types.NamespacedName{Name: "ws-bb-stale", Namespace: ns}

	r := newTestReconciler()
	r.ChangedFilesReporter = &fakeChangedFilesReporter{files: []string{"internal/auth/login.go"}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile with a reachable supervisor: %v", err)
	}

	r.ChangedFilesReporter = &fakeChangedFilesReporter{err: context.DeadlineExceeded}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile with an unreachable supervisor: %v", err)
	}

	entry := blackboardOf(t, ctx, req)
	if entry == nil || !slices.Equal(entry.ChangedFiles, []string{"internal/auth/login.go"}) {
		t.Fatalf("changedFiles = %v, want the last known list retained", blackboardOf(t, ctx, req).ChangedFiles)
	}
}

// 10.8: a destroyed workspace's branch is no longer being worked on, so its
// entry stops being one the other branches have to reckon with.
func TestReconcile_MarksBlackboardEntryInactiveOnDestroy(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-destroy", "feature/bb-destroy", "認証基盤の刷新", "")
	req := types.NamespacedName{Name: "ws-bb-destroy", Namespace: ns}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	r := newTestReconciler()
	// Held unconfirmed so the object survives the reconcile for these
	// assertions (the same reason lifecycle_test.go's destroy test does).
	r.EvacuationConfirmer = &fakeEvacuationConfirmer{complete: false}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile (terminating): %v", err)
	}

	entry := blackboardOf(t, ctx, req)
	if entry == nil {
		t.Fatal("status.blackboard is nil after destroy, want the entry retained but inactive")
	}
	if entry.Active {
		t.Error("active = true after destroy, want false (10.8)")
	}
}

// The verification 8.1 asks for: after several workspaces come and go, only
// the live branches are the ones a reader sees as valid.
func TestActiveBlackboardEntries_ExcludesDestroyedBranches(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-live", "feature/live", "生存側", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-gone", "feature/gone", "破棄側", "")

	gone := types.NamespacedName{Name: "ws-bb-gone", Namespace: ns}
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, gone, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}
	r := newTestReconciler()
	r.EvacuationConfirmer = &fakeEvacuationConfirmer{complete: false}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: gone}); err != nil {
		t.Fatalf("reconcile (terminating): %v", err)
	}

	entries, err := ActiveBlackboardEntries(ctx, testClient, ns)
	if err != nil {
		t.Fatalf("ActiveBlackboardEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Branch != "feature/live" {
		t.Fatalf("active entries = %+v, want only feature/live", entries)
	}
}

func TestSupervisorChangedFilesReporter_ReadsFromSupervisor(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"files":["cmd/main.go"]}`))
	}))
	defer srv.Close()
	host, portStr, err := splitHostPort(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	ws := &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-bb-http", Namespace: ns},
		Spec:       devplatformv1alpha1.WorkspaceSpec{Repository: "https://example.com/r.git", Branch: "feature/bb-http", TemplateRef: "default"},
	}
	ws.Status.WorkspaceId = "ws-bb-http"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-bb-http-0", Namespace: ns, Labels: workspaceLabels(ws)},
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

	files, err := (&SupervisorChangedFilesReporter{Client: testClient, Port: port}).ChangedFiles(ctx, ws)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if gotPath != "/changed-files" {
		t.Errorf("path = %q, want /changed-files", gotPath)
	}
	if !slices.Equal(files, []string{"cmd/main.go"}) {
		t.Fatalf("files = %v, want [cmd/main.go]", files)
	}
}

// 10.5's shared prefix only stays identical while every workspace renders the
// entries in the same order, and the API's own listing order (by object name)
// is not that order.
func TestActiveBlackboardEntries_AreOrderedByBranch(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-order-1", "feature/order-c", "c", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-order-2", "feature/order-a", "a", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-bb-order-3", "feature/order-b", "b", "")

	entries, err := ActiveBlackboardEntries(ctx, testClient, ns)
	if err != nil {
		t.Fatalf("ActiveBlackboardEntries: %v", err)
	}
	var branches []string
	for _, e := range entries {
		branches = append(branches, e.Branch)
	}
	want := []string{"feature/order-a", "feature/order-b", "feature/order-c"}
	if !slices.Equal(branches, want) {
		t.Errorf("branches = %v, want %v", branches, want)
	}
}
