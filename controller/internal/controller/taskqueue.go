package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// defaultMaxConcurrentTasks is a conservative stand-in for the operator's
	// setting; the real value comes from values.yaml via MAX_CONCURRENT_TASKS.
	defaultMaxConcurrentTasks = 2

	// maxRecentQuotaHits bounds the history Stats reports (7.12).
	maxRecentQuotaHits = 20

	conditionDispatched = "Dispatched"

	// quotaRecheckInterval paces the retry while a quota window is spent. The
	// reading carries a reset time, but polling on a fixed interval costs one
	// cheap file read per workspace and needs no clock agreement.
	quotaRecheckInterval = 5 * time.Minute
)

// QuotaScope names which of Claude Code's quota windows was exhausted. The
// distinction decides whether the governor switches model or stops dispatching
// altogether (7.6/7.7/7.8).
type QuotaScope string

const (
	QuotaScopeModel   QuotaScope = "model"
	QuotaScopeSession QuotaScope = "session"
	QuotaScopeWeekly  QuotaScope = "weekly"
)

// QuotaHit is one observed quota exhaustion.
type QuotaHit struct {
	Scope QuotaScope
	At    time.Time
}

// QueueStats answers 7.12.
type QueueStats struct {
	Queued          int
	Running         int
	RecentQuotaHits []QuotaHit
}

// TaskQueueReconciler is the Task Queue and Quota Governor's dispatch loop. It
// keeps the number of tasks occupying a workspace session at or below the
// operator's limit and releases queued tasks as slots free up (7.1/7.2).
type TaskQueueReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MaxConcurrent is the operator-configured ceiling on simultaneously
	// running Claude Code tasks. Non-positive falls back to
	// defaultMaxConcurrentTasks.
	MaxConcurrent int

	// UsageObserver supplies the structured quota reading the dispatch
	// decision rests on (7.11). Unset disables quota-based throttling.
	UsageObserver UsageObserver

	// MinRemainingPercent is the balance below which a quota window counts as
	// spent. Zero throttles only once a window is actually gone; raise it to
	// leave headroom for work already in flight.
	MinRemainingPercent float64

	// ModelFallbacks is the switching order used when a model-scoped window
	// runs out (7.7), most preferred first. The end of the chain turns the
	// model window into an ordinary stop.
	ModelFallbacks []string

	// Notify reports a stop a human has to act on. Unset is a no-op: chat
	// relay is the platform's only notification path and losing it must not
	// stop reconciliation.
	Notify func(ctx context.Context, ws *devplatformv1alpha1.Workspace, kind, detail string)

	// Now overrides time.Now; tests may fake it.
	Now func() time.Time

	// stopped is the condition dispatch is currently halted on, empty when
	// running. It is in-process because it is derived: the next reading
	// re-establishes it, and a control plane that restarts into a spent
	// quota simply observes it again before dispatching anything.
	stopped string

	// recentQuotaHits is deliberately in-process: only the queue itself has to
	// survive a restart (7.2), and rebuilding this history costs one quota
	// observation rather than a persisted resource.
	mu              sync.Mutex
	recentQuotaHits []QuotaHit
}

func (r *TaskQueueReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *TaskQueueReconciler) maxConcurrent() int {
	if r.MaxConcurrent > 0 {
		return r.MaxConcurrent
	}
	return defaultMaxConcurrentTasks
}

// Reconcile runs one pass over the whole queue rather than over the triggering
// object alone: a slot frees when some *other* TaskRequest leaves a running
// phase, and that event carries no reference to the tasks it unblocks.
// SetupWithManager pins this to a single worker, which is what keeps the
// concurrency count free of races (design.md: "ディスパッチ判断は単一ループで行い").
func (r *TaskQueueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var list devplatformv1alpha1.TaskRequestList
	if err := r.List(ctx, &list, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	running := 0
	var pending []devplatformv1alpha1.TaskRequest
	for _, task := range list.Items {
		if !task.DeletionTimestamp.IsZero() {
			continue
		}
		switch task.Status.Phase {
		case devplatformv1alpha1.TaskPhaseRunning,
			devplatformv1alpha1.TaskPhaseVerifying,
			devplatformv1alpha1.TaskPhaseHumanIntervention:
			running++
		case "", devplatformv1alpha1.TaskPhasePending:
			pending = append(pending, task)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		return isOlder(pending[i].CreationTimestamp, pending[i].Name, pending[j].CreationTimestamp, pending[j].Name)
	})

	limit := r.maxConcurrent()
	for i := range pending {
		task := &pending[i]

		if running >= limit {
			if err := r.hold(ctx, task, "ConcurrencyLimit", fmt.Sprintf("%d of %d task slots in use", running, limit)); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}

		// A task blocked on its own workspace must not stall the queue behind
		// it: the slot it would have taken stays available to the next task.
		ready, err := r.workspaceReady(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			if err := r.hold(ctx, task, "WorkspaceNotReady", fmt.Sprintf("workspace %q is not Ready", task.Spec.WorkspaceRef)); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}

		if reading, ok := r.observe(ctx, task); ok {
			stop, err := r.throttle(ctx, task, reading)
			if err != nil {
				return ctrl.Result{}, err
			}
			if stop != "" {
				// The credential and the account-wide windows are shared, so a
				// condition that stops one task stops the whole queue behind it.
				if err := r.holdAll(ctx, pending[i:], stop, r.stopped); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: quotaRecheckInterval}, nil
			}
		}

		if err := r.dispatch(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		running++
	}

	return ctrl.Result{}, nil
}

// observe reads the quota for the workspace a task would run in. A failed read
// reports false rather than an error: the reading throttles dispatch, and an
// unreachable supervisor must not also stop it.
func (r *TaskQueueReconciler) observe(ctx context.Context, task *devplatformv1alpha1.TaskRequest) (UsageReading, bool) {
	if r.UsageObserver == nil {
		return UsageReading{}, false
	}
	var ws devplatformv1alpha1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.WorkspaceRef, Namespace: task.Namespace}, &ws); err != nil {
		return UsageReading{}, false
	}
	reading, err := r.UsageObserver.ObserveUsage(ctx, &ws)
	if err != nil {
		log.FromContext(ctx).Info("quota reading unavailable, dispatching without it",
			"workspace", ws.Name, "error", err)
		return UsageReading{}, false
	}
	return reading, true
}

// throttle applies one reading to the queue, returning the hold reason when
// dispatch must stop and the empty string when the task may go ahead. It is
// where 7.7 (switch model and continue), 7.8 (stop, notify, wait) and 7.9
// (stop everything on a credential failure) diverge.
func (r *TaskQueueReconciler) throttle(ctx context.Context, task *devplatformv1alpha1.TaskRequest, reading UsageReading) (string, error) {
	if isAuthError(reading.ErrorKind) {
		r.stop(ctx, task, EventAuthError, fmt.Sprintf("submission stopped for every workspace: %s", reading.ErrorKind))
		return "AuthError", nil
	}

	scope, spent := exhaustedScope(reading.Snapshots, r.MinRemainingPercent)
	if !spent {
		r.resume()
		return "", nil
	}
	r.recordQuotaHit(scope, r.now())

	if scope == QuotaScopeModel {
		next, err := r.switchModel(ctx, task)
		if err != nil {
			return "", err
		}
		if next != "" {
			r.resume()
			return "", nil
		}
		// Nothing left to switch to, so the model window is as binding as an
		// account-wide one.
	}

	r.stop(ctx, task, EventQuotaExhausted, fmt.Sprintf("dispatch stopped: the %s quota window is spent", scope))
	return "QuotaExhausted", nil
}

// switchModel moves the workspace to the next model in the chain and reports
// it, or the empty string when the chain is spent. The model is recorded as an
// annotation rather than status because the Workspace reconciler owns status;
// that reconciler reads it back and rewrites the session's ANTHROPIC_MODEL.
func (r *TaskQueueReconciler) switchModel(ctx context.Context, task *devplatformv1alpha1.TaskRequest) (string, error) {
	var ws devplatformv1alpha1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.WorkspaceRef, Namespace: task.Namespace}, &ws); err != nil {
		return "", client.IgnoreNotFound(err)
	}

	next := nextModel(r.ModelFallbacks, ws.Annotations[AnnotationActiveModel])
	if next == "" {
		return "", nil
	}
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[AnnotationActiveModel] = next
	if err := r.Update(ctx, &ws); err != nil {
		return "", err
	}
	log.FromContext(ctx).Info("model quota spent, switching model", "workspace", ws.Name, "model", next)
	return next, nil
}

// nextModel returns the entry after current in the chain. An unset current
// means the workspace is still on the chain's head, so the switch goes to the
// second entry; a current that is not in the chain has nowhere defined to go.
func nextModel(chain []string, current string) string {
	at := 0
	if current != "" {
		at = -1
		for i, m := range chain {
			if m == current {
				at = i
				break
			}
		}
		if at < 0 {
			return ""
		}
	}
	if at+1 >= len(chain) {
		return ""
	}
	return chain[at+1]
}

// stop halts dispatch and announces it once. Re-announcing every pass would
// bury the recovery notice under repeats of the same stop, so the detail is
// what marks the condition as already reported.
func (r *TaskQueueReconciler) stop(ctx context.Context, task *devplatformv1alpha1.TaskRequest, kind, detail string) {
	if r.stopped == detail {
		return
	}
	r.stopped = detail
	log.FromContext(ctx).Info("dispatch stopped", "kind", kind, "detail", detail)
	if r.Notify == nil {
		return
	}
	var ws devplatformv1alpha1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.WorkspaceRef, Namespace: task.Namespace}, &ws); err != nil {
		ws.Name = task.Spec.WorkspaceRef
	}
	r.Notify(ctx, &ws, kind, detail)
}

func (r *TaskQueueReconciler) resume() {
	r.stopped = ""
}

// holdAll parks the rest of the queue under one reason.
func (r *TaskQueueReconciler) holdAll(ctx context.Context, tasks []devplatformv1alpha1.TaskRequest, reason, message string) error {
	for i := range tasks {
		if err := r.hold(ctx, &tasks[i], reason, message); err != nil {
			return err
		}
	}
	return nil
}

func (r *TaskQueueReconciler) workspaceReady(ctx context.Context, task *devplatformv1alpha1.TaskRequest) (bool, error) {
	var ws devplatformv1alpha1.Workspace
	err := r.Get(ctx, types.NamespacedName{Name: task.Spec.WorkspaceRef, Namespace: task.Namespace}, &ws)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ws.Status.Phase == devplatformv1alpha1.WorkspacePhaseReady, nil
}

func (r *TaskQueueReconciler) dispatch(ctx context.Context, task *devplatformv1alpha1.TaskRequest) error {
	task.Status.Phase = devplatformv1alpha1.TaskPhaseRunning
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:    conditionDispatched,
		Status:  metav1.ConditionTrue,
		Reason:  "SlotAvailable",
		Message: "dispatched to its workspace session",
	})
	return r.Status().Update(ctx, task)
}

// hold records why a task is still queued, writing only when the reason
// actually changed so a full queue does not generate a status update per task
// per pass.
func (r *TaskQueueReconciler) hold(ctx context.Context, task *devplatformv1alpha1.TaskRequest, reason, message string) error {
	task.Status.Phase = devplatformv1alpha1.TaskPhasePending
	changed := meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:    conditionDispatched,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, task)
}

// recordQuotaHit appends an observed quota exhaustion to the bounded history
// Stats reports.
func (r *TaskQueueReconciler) recordQuotaHit(scope QuotaScope, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recentQuotaHits = append(r.recentQuotaHits, QuotaHit{Scope: scope, At: at})
	if len(r.recentQuotaHits) > maxRecentQuotaHits {
		r.recentQuotaHits = r.recentQuotaHits[len(r.recentQuotaHits)-maxRecentQuotaHits:]
	}
}

// Stats reports queue depth, running count and recent quota hits (7.12). The
// counts are read from the API server, so they describe the queue as it
// actually is rather than what this process happens to remember.
func (r *TaskQueueReconciler) Stats(ctx context.Context, namespace string) (QueueStats, error) {
	var list devplatformv1alpha1.TaskRequestList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return QueueStats{}, err
	}

	var stats QueueStats
	for _, task := range list.Items {
		if !task.DeletionTimestamp.IsZero() {
			continue
		}
		switch task.Status.Phase {
		case devplatformv1alpha1.TaskPhaseRunning,
			devplatformv1alpha1.TaskPhaseVerifying,
			devplatformv1alpha1.TaskPhaseHumanIntervention:
			stats.Running++
		case "", devplatformv1alpha1.TaskPhasePending:
			stats.Queued++
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	stats.RecentQuotaHits = append([]QuotaHit(nil), r.recentQuotaHits...)
	return stats, nil
}

// SetupWithManager wires the dispatch loop into a controller-runtime Manager.
// Workspace is watched too: a workspace reaching Ready is what unblocks the
// tasks held on it, and that event never touches a TaskRequest.
func (r *TaskQueueReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devplatformv1alpha1.TaskRequest{}).
		Watches(&devplatformv1alpha1.Workspace{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				// Reconcile ignores the object name, so every workspace event
				// collapses into one queue pass for that namespace.
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: "queue"}}}
			})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
