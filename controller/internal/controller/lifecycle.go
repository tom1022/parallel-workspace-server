// This file covers task 2.6: the Suspended/resume/Terminating lifecycle
// (design.md "Workspace Controller" State Management / Implementation Notes,
// Requirement 13.1-13.8). Idle-detection and the destroy-candidate retention
// check both key off status.lastActivityAt and the Suspended condition's own
// LastTransitionTime rather than a dedicated timestamp field, so no CRD
// schema change is needed.
package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// defaultIdleSuspendTimeout is D-2's idle-detection half: connection and
	// task absence for this long moves Ready -> Suspended (13.1).
	defaultIdleSuspendTimeout = 30 * time.Minute
	// defaultSuspendedRetentionPeriod is D-2's retention half: staying
	// Suspended past this marks the workspace a destroy candidate (13.8).
	defaultSuspendedRetentionPeriod = 7 * 24 * time.Hour

	// idleCheckInterval paces the Ready/Suspended requeue loop. Both D-2
	// windows are tens of minutes to days long, so this is far coarser than
	// provisioning's pollInterval (which exists for a one-off, short wait).
	idleCheckInterval = time.Minute

	// conditionSuspended's LastTransitionTime doubles as "when this workspace
	// became Suspended" so 13.8's retention check needs no dedicated status
	// field.
	conditionSuspended = "Suspended"
	// conditionResumeRequested is set by MarkActivity when it observes a
	// Suspended workspace: it is the resume trigger 13.3 describes as a
	// connection request or task submission, decoupled from spec.desiredPhase
	// (which stays at its caller-set value and is not itself a reliable
	// "please resume" signal, since its zero value already means Ready).
	conditionResumeRequested = "ResumeRequested"
	// conditionDestroyCandidate marks 13.8's destroy-candidate notification.
	conditionDestroyCandidate = "DestroyCandidate"
)

// MarkActivity records fresh activity against ws — a Terminal Gateway
// connection or a Task Queue dispatch (design.md "State Management": 4.8) —
// updating the idle-detection clock reconcileReady/reconcileSuspended read.
// Terminal Gateway and Task Queue are separate components (task 3+); this
// method is their write-side entry point into the Workspace Controller,
// which stays the sole writer of Workspace status. Calling it while ws is
// Suspended is itself the resume request (13.3): the next Suspended-phase
// reconcile scales the StatefulSet back up.
func (r *WorkspaceReconciler) MarkActivity(ctx context.Context, ws *devplatformv1alpha1.Workspace) error {
	now := metav1.NewTime(r.now())
	ws.Status.LastActivityAt = &now
	if ws.Status.Phase == devplatformv1alpha1.WorkspacePhaseSuspended {
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:    conditionResumeRequested,
			Status:  metav1.ConditionTrue,
			Reason:  "ActivityDetected",
			Message: "a connection or task request was received while suspended",
		})
	}
	return r.Status().Update(ctx, ws)
}

// reconcileReady is Ready's idle-detection extension point (13.1): once both
// connection and task activity have been absent for defaultIdleSuspendTimeout
// — or a caller explicitly requests it via spec.desiredPhase — it suspends
// the workspace.
func (r *WorkspaceReconciler) reconcileReady(ctx context.Context, ws *devplatformv1alpha1.Workspace) (ctrl.Result, error) {
	idle := ws.Status.LastActivityAt != nil && r.now().Sub(ws.Status.LastActivityAt.Time) >= defaultIdleSuspendTimeout
	requested := ws.Spec.DesiredPhase == devplatformv1alpha1.DesiredPhaseSuspended
	if !idle && !requested {
		return ctrl.Result{RequeueAfter: idleCheckInterval}, nil
	}

	resourceName := ws.Status.WorkspaceId
	if resourceName != "" {
		if err := r.scaleStatefulSet(ctx, ws.Namespace, resourceName, 0); err != nil {
			return ctrl.Result{}, err
		}
	}

	reason := "IdleTimeout"
	if requested {
		reason = "SuspendRequested"
	}
	ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseSuspended
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               conditionSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            "execution environment stopped; working directory retained (13.2)",
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	return ctrl.Result{}, r.Status().Update(ctx, ws)
}

// reconcileSuspended handles resume (via MarkActivity's ResumeRequested
// signal, 13.3) and flags long-idle workspaces as destroy candidates (13.8).
func (r *WorkspaceReconciler) reconcileSuspended(ctx context.Context, ws *devplatformv1alpha1.Workspace) (ctrl.Result, error) {
	if meta.IsStatusConditionTrue(ws.Status.Conditions, conditionResumeRequested) {
		return r.reconcileResume(ctx, ws)
	}

	if cond := meta.FindStatusCondition(ws.Status.Conditions, conditionSuspended); cond != nil && cond.Status == metav1.ConditionTrue {
		alreadyFlagged := meta.IsStatusConditionTrue(ws.Status.Conditions, conditionDestroyCandidate)
		if !alreadyFlagged && r.now().Sub(cond.LastTransitionTime.Time) >= defaultSuspendedRetentionPeriod {
			meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
				Type:    conditionDestroyCandidate,
				Status:  metav1.ConditionTrue,
				Reason:  "RetentionPeriodExceeded",
				Message: fmt.Sprintf("suspended for longer than the %s retention period; candidate for destruction (13.8)", defaultSuspendedRetentionPeriod),
			})
			if err := r.Status().Update(ctx, ws); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{RequeueAfter: idleCheckInterval}, nil
}

// reconcileResume scales the StatefulSet back to 1 and reconnects to the
// retained PVC. Readiness is judged from the StatefulSet's current state,
// never from having been Ready before (13.5).
func (r *WorkspaceReconciler) reconcileResume(ctx context.Context, ws *devplatformv1alpha1.Workspace) (ctrl.Result, error) {
	resourceName := ws.Status.WorkspaceId
	if resourceName == "" {
		return ctrl.Result{}, nil
	}
	if err := r.scaleStatefulSet(ctx, ws.Namespace, resourceName, 1); err != nil {
		return ctrl.Result{}, err
	}

	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ws.Namespace}, &sts); err != nil {
		return ctrl.Result{}, err
	}
	if sts.Status.ReadyReplicas < 1 {
		return ctrl.Result{RequeueAfter: idleCheckInterval}, nil
	}

	ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseReady
	ws.Status.SessionId = resourceName + "-session" // 13.4: execution session re-created
	now := metav1.NewTime(r.now())
	ws.Status.LastActivityAt = &now
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               conditionSuspended,
		Status:             metav1.ConditionFalse,
		Reason:             "Resumed",
		Message:            "workspace resumed from suspension",
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	meta.RemoveStatusCondition(&ws.Status.Conditions, conditionResumeRequested)
	meta.RemoveStatusCondition(&ws.Status.Conditions, conditionDestroyCandidate)
	return ctrl.Result{}, r.Status().Update(ctx, ws)
}

func (r *WorkspaceReconciler) scaleStatefulSet(ctx context.Context, namespace, name string, replicas int32) error {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == replicas {
		return nil
	}
	sts.Spec.Replicas = &replicas
	return r.Update(ctx, &sts)
}

// reconcileTerminating runs whenever ws carries a DeletionTimestamp (an
// explicit destroy request, 13.6/13.7), regardless of which phase it was
// destroyed from (Ready/Suspended/Failed). Compute (StatefulSet) and the
// database are torn down immediately regardless of evacuation status — only
// the PVC-delete step is gated on EvacuationConfirmer (task 2.7, 16.4), via
// evacuationFinalizer staying on ws until that gate passes. IngressRoutes are
// left to Kubernetes' own garbage collector (11.7) instead of being deleted
// here.
func (r *WorkspaceReconciler) reconcileTerminating(ctx context.Context, ws *devplatformv1alpha1.Workspace) (ctrl.Result, error) {
	resourceName := ws.Status.WorkspaceId

	// A workspace that only ever mirrored another canonical workspace's
	// substrate (1.6) owns no StatefulSet/PVC/Database of its own to tear
	// down — resourceName here is the *canonical's* id, and deleting by it
	// would reach into that still-live workspace's substrate out from under
	// it (16.1/16.2: one working directory, never shared).
	if resourceName == "" || meta.IsStatusConditionTrue(ws.Status.Conditions, conditionDuplicateOfExisting) {
		return ctrl.Result{}, r.removeEvacuationFinalizer(ctx, ws)
	}

	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseTerminating {
		ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseTerminating
		if err := r.Status().Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := client.IgnoreNotFound(r.Delete(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ws.Namespace},
	})); err != nil {
		return ctrl.Result{}, err
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, databaseRef(ws.Namespace, resourceName))); err != nil {
		return ctrl.Result{}, err
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, databaseRoleRef(ws.Namespace, resourceName))); err != nil {
		return ctrl.Result{}, err
	}

	complete, err := r.evacuationComplete(ctx, ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !complete {
		return ctrl.Result{RequeueAfter: evacuationPollInterval}, nil
	}

	if err := client.IgnoreNotFound(r.Delete(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ws.Namespace},
	})); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.removeEvacuationFinalizer(ctx, ws)
}
