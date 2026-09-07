package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// createReadyWorkspace makes a Workspace the queue's dispatch precondition
// ("対象ワークスペースが Ready であること") accepts, without running the
// provisioning reconciler.
func createReadyWorkspace(t *testing.T, ctx context.Context, ns, name string) {
	t.Helper()
	ws := &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: devplatformv1alpha1.WorkspaceSpec{
			Repository:  "https://example.com/repo.git",
			Branch:      name,
			TemplateRef: "default",
		},
	}
	if err := testClient.Create(ctx, ws); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseReady
	if err := testClient.Status().Update(ctx, ws); err != nil {
		t.Fatalf("mark Workspace Ready: %v", err)
	}
}

func createTask(t *testing.T, ctx context.Context, ns, name, workspace string) {
	t.Helper()
	task := &devplatformv1alpha1.TaskRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       devplatformv1alpha1.TaskRequestSpec{WorkspaceRef: workspace},
	}
	if err := testClient.Create(ctx, task); err != nil {
		t.Fatalf("create TaskRequest %s: %v", name, err)
	}
}

func setTaskPhase(t *testing.T, ctx context.Context, ns, name string, phase devplatformv1alpha1.TaskPhase) {
	t.Helper()
	var task devplatformv1alpha1.TaskRequest
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &task); err != nil {
		t.Fatalf("get TaskRequest %s: %v", name, err)
	}
	task.Status.Phase = phase
	if err := testClient.Status().Update(ctx, &task); err != nil {
		t.Fatalf("set phase of %s: %v", name, err)
	}
}

func taskPhases(t *testing.T, ctx context.Context, ns string) map[string]devplatformv1alpha1.TaskPhase {
	t.Helper()
	var list devplatformv1alpha1.TaskRequestList
	if err := testClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list TaskRequests: %v", err)
	}
	out := map[string]devplatformv1alpha1.TaskPhase{}
	for _, task := range list.Items {
		out[task.Name] = task.Status.Phase
	}
	return out
}

func countPhase(phases map[string]devplatformv1alpha1.TaskPhase, want devplatformv1alpha1.TaskPhase) int {
	n := 0
	for _, p := range phases {
		if p == want {
			n++
		}
	}
	return n
}

// runQueue drives one dispatch pass. The reconciler re-evaluates the whole
// queue on any event, so the triggering object's identity is irrelevant.
func runQueue(t *testing.T, ctx context.Context, r *TaskQueueReconciler, ns string) {
	t.Helper()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "any"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// The acceptance criterion for 7.1: more tasks than slots must leave running
// at the limit and the excess in the queue.
func TestTaskQueue_HoldsExcessTasksAtConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createReadyWorkspace(t, ctx, ns, "ws-a")

	for i := 0; i < 5; i++ {
		createTask(t, ctx, ns, fmt.Sprintf("task-%d", i), "ws-a")
	}

	r := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 2}
	runQueue(t, ctx, r, ns)

	phases := taskPhases(t, ctx, ns)
	if got := countPhase(phases, devplatformv1alpha1.TaskPhaseRunning); got != 2 {
		t.Errorf("running = %d, want 2 (concurrency limit): %v", got, phases)
	}
	if got := countPhase(phases, devplatformv1alpha1.TaskPhasePending); got != 3 {
		t.Errorf("pending = %d, want 3 (excess stays queued): %v", got, phases)
	}

	// Repeated passes must not creep past the limit.
	runQueue(t, ctx, r, ns)
	if got := countPhase(taskPhases(t, ctx, ns), devplatformv1alpha1.TaskPhaseRunning); got != 2 {
		t.Errorf("running after second pass = %d, want 2", got)
	}
}

func TestTaskQueue_DispatchesOldestFirst(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createReadyWorkspace(t, ctx, ns, "ws-a")

	// The API server stamps creationTimestamp itself, at second granularity,
	// so arrival order can only be established by actually waiting. The names
	// are inverted: a pass that sorted by name would dispatch task-a instead.
	createTask(t, ctx, ns, "task-b", "ws-a")
	time.Sleep(1100 * time.Millisecond)
	createTask(t, ctx, ns, "task-a", "ws-a")

	r := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 1}
	runQueue(t, ctx, r, ns)

	phases := taskPhases(t, ctx, ns)
	if phases["task-b"] != devplatformv1alpha1.TaskPhaseRunning || phases["task-a"] != devplatformv1alpha1.TaskPhasePending {
		t.Errorf("dispatched out of arrival order: %v", phases)
	}
}

func TestTaskQueue_DispatchesQueuedTaskWhenSlotFrees(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createReadyWorkspace(t, ctx, ns, "ws-a")

	createTask(t, ctx, ns, "task-0", "ws-a")
	createTask(t, ctx, ns, "task-1", "ws-a")
	createTask(t, ctx, ns, "task-2", "ws-a")

	r := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 1}
	runQueue(t, ctx, r, ns)

	phases := taskPhases(t, ctx, ns)
	if got := countPhase(phases, devplatformv1alpha1.TaskPhaseRunning); got != 1 {
		t.Fatalf("running = %d, want 1: %v", got, phases)
	}
	var dispatched string
	for name, phase := range phases {
		if phase == devplatformv1alpha1.TaskPhaseRunning {
			dispatched = name
		}
	}

	setTaskPhase(t, ctx, ns, dispatched, devplatformv1alpha1.TaskPhaseCompleted)
	runQueue(t, ctx, r, ns)

	phases = taskPhases(t, ctx, ns)
	if got := countPhase(phases, devplatformv1alpha1.TaskPhaseRunning); got != 1 {
		t.Errorf("running after a slot freed = %d, want 1: %v", got, phases)
	}
	if got := countPhase(phases, devplatformv1alpha1.TaskPhasePending); got != 1 {
		t.Errorf("pending after a slot freed = %d, want 1: %v", got, phases)
	}
}

// Verifying and HumanIntervention still occupy the workspace's session, so
// they must keep holding their slot.
func TestTaskQueue_NonTerminalPhasesKeepTheirSlot(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createReadyWorkspace(t, ctx, ns, "ws-a")

	createTask(t, ctx, ns, "task-0", "ws-a")
	createTask(t, ctx, ns, "task-1", "ws-a")

	r := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 1}
	runQueue(t, ctx, r, ns)

	for _, phase := range []devplatformv1alpha1.TaskPhase{
		devplatformv1alpha1.TaskPhaseVerifying,
		devplatformv1alpha1.TaskPhaseHumanIntervention,
	} {
		setTaskPhase(t, ctx, ns, "task-0", phase)
		runQueue(t, ctx, r, ns)
		if got := taskPhases(t, ctx, ns)["task-1"]; got != devplatformv1alpha1.TaskPhasePending {
			t.Errorf("with task-0 in %s, task-1 = %q, want Pending", phase, got)
		}
	}
}

func TestTaskQueue_HoldsTaskWhoseWorkspaceIsNotReady(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createReadyWorkspace(t, ctx, ns, "ws-ready")

	ws := &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-provisioning", Namespace: ns},
		Spec: devplatformv1alpha1.WorkspaceSpec{
			Repository:  "https://example.com/repo.git",
			Branch:      "provisioning",
			TemplateRef: "default",
		},
	}
	if err := testClient.Create(ctx, ws); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}

	createTask(t, ctx, ns, "task-blocked", "ws-provisioning")
	createTask(t, ctx, ns, "task-ok", "ws-ready")
	createTask(t, ctx, ns, "task-missing", "ws-absent")

	r := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 3}
	runQueue(t, ctx, r, ns)

	phases := taskPhases(t, ctx, ns)
	if phases["task-blocked"] != devplatformv1alpha1.TaskPhasePending {
		t.Errorf("task-blocked = %q, want Pending (workspace not Ready)", phases["task-blocked"])
	}
	if phases["task-missing"] != devplatformv1alpha1.TaskPhasePending {
		t.Errorf("task-missing = %q, want Pending (workspace absent)", phases["task-missing"])
	}
	// A task blocked on its own workspace must not stall the tasks behind it.
	if phases["task-ok"] != devplatformv1alpha1.TaskPhaseRunning {
		t.Errorf("task-ok = %q, want Running", phases["task-ok"])
	}

	var blocked devplatformv1alpha1.TaskRequest
	if err := testClient.Get(ctx, types.NamespacedName{Name: "task-blocked", Namespace: ns}, &blocked); err != nil {
		t.Fatalf("get task-blocked: %v", err)
	}
	if reason := heldReason(blocked); reason != "WorkspaceNotReady" {
		t.Errorf("hold reason = %q, want WorkspaceNotReady", reason)
	}
}

func TestTaskQueue_RecordsConcurrencyHoldReason(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createReadyWorkspace(t, ctx, ns, "ws-a")

	createTask(t, ctx, ns, "task-0", "ws-a")
	createTask(t, ctx, ns, "task-1", "ws-a")

	r := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 1}
	runQueue(t, ctx, r, ns)

	var held devplatformv1alpha1.TaskRequest
	if err := testClient.Get(ctx, types.NamespacedName{Name: "task-1", Namespace: ns}, &held); err != nil {
		t.Fatalf("get task-1: %v", err)
	}
	if reason := heldReason(held); reason != "ConcurrencyLimit" {
		t.Errorf("hold reason = %q, want ConcurrencyLimit", reason)
	}
}

// 7.12: queue depth, running count and recent quota hits must be queryable,
// and the counts must come from the API server rather than in-process
// bookkeeping so a restarted control plane reports the same queue.
func TestTaskQueue_StatsSurviveControlPlaneRestart(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createReadyWorkspace(t, ctx, ns, "ws-a")

	for i := 0; i < 4; i++ {
		createTask(t, ctx, ns, fmt.Sprintf("task-%d", i), "ws-a")
	}

	r := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 2}
	runQueue(t, ctx, r, ns)
	r.recordQuotaHit(QuotaScopeWeekly, time.Now())

	stats, err := r.Stats(ctx, ns)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Running != 2 || stats.Queued != 2 {
		t.Errorf("stats = running %d / queued %d, want 2 / 2", stats.Running, stats.Queued)
	}
	if len(stats.RecentQuotaHits) != 1 || stats.RecentQuotaHits[0].Scope != QuotaScopeWeekly {
		t.Errorf("recent quota hits = %v, want one weekly hit", stats.RecentQuotaHits)
	}

	// A fresh reconciler stands in for a restarted control plane: the queue is
	// in the API server, so it must still be there.
	restarted := &TaskQueueReconciler{Client: testClient, MaxConcurrent: 2}
	stats, err = restarted.Stats(ctx, ns)
	if err != nil {
		t.Fatalf("stats after restart: %v", err)
	}
	if stats.Running != 2 || stats.Queued != 2 {
		t.Errorf("stats after restart = running %d / queued %d, want 2 / 2", stats.Running, stats.Queued)
	}

	runQueue(t, ctx, restarted, ns)
	if got := countPhase(taskPhases(t, ctx, ns), devplatformv1alpha1.TaskPhaseRunning); got != 2 {
		t.Errorf("running after restart = %d, want 2 (no double dispatch)", got)
	}
}

func heldReason(task devplatformv1alpha1.TaskRequest) string {
	for _, c := range task.Status.Conditions {
		if c.Type == conditionDispatched && c.Status == metav1.ConditionFalse {
			return c.Reason
		}
	}
	return ""
}
