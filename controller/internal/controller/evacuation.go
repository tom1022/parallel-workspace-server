// This file covers task 2.7: the evacuation-wait Finalizer (design.md
// "Workspace Controller" Responsibilities: "破棄以外の経路で PVC を削除し
// ない。Finalizer により、退避完了を確認するまで削除処理を完了させない"
// — 13.6/16.4). It only confirms evacuation completion; performing the
// evacuation itself is the separate Evacuation Agent's job (task 4,
// design.md "Evacuation Agent" — 16.6-16.9).
package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// evacuationFinalizer holds a Workspace object around after Delete until
	// evacuation is confirmed, so reconcileTerminating gets a chance to gate
	// the PVC-delete step on it instead of the object simply vanishing.
	evacuationFinalizer = "workspace.tom1022.github.io/evacuation"

	// evacuationPollInterval paces the Terminating requeue loop while
	// evacuation is still in flight.
	evacuationPollInterval = 10 * time.Second

	// conditionEvacuated carries the reason a stop was refused, so an
	// unreachable destination is visible on the object rather than only in the
	// notification stream.
	conditionEvacuated = "Evacuated"

	// EventEvacuationFailed is the notification kind reported when a workspace
	// cannot be stopped because its working directory could not be captured.
	EventEvacuationFailed = "EvacuationFailed"
)

// EvacuationConfirmer is the boundary to the Evacuation Agent's off-node
// backup (design.md "Workspace Controller" Dependencies: "External: Garage
// — 退避完了の確認(P1)"; "Evacuation Agent" Implementation Notes:
// "Workspace Controller の Finalizer は、破棄の完了前に最新の
// EvacuationSnapshot の存在を確認する"). It only answers whether ws's most
// recent working-directory state has already been captured off-node — it
// never triggers or performs the capture itself.
//
// Production wiring (main.go) plugs in a Garage-backed implementation once
// task 4's Evacuation Agent exists; tests inject a fake. No default: an
// unconfigured EvacuationConfirmer fails destroy explicitly (evacuationComplete
// below) rather than silently skipping the wait and deleting the PVC anyway.
type EvacuationConfirmer interface {
	IsEvacuationComplete(ctx context.Context, ws *devplatformv1alpha1.Workspace) (bool, error)
}

// evacuationComplete asks EvacuationConfirmer whether ws's PVC is safe to
// actually delete.
func (r *WorkspaceReconciler) evacuationComplete(ctx context.Context, ws *devplatformv1alpha1.Workspace) (bool, error) {
	if r.EvacuationConfirmer == nil {
		return false, fmt.Errorf("devplatform: no EvacuationConfirmer configured")
	}
	return r.EvacuationConfirmer.IsEvacuationComplete(ctx, ws)
}

// removeEvacuationFinalizer releases ws for actual deletion: either it never
// owned substrate to begin with (no WorkspaceId, or it only ever mirrored a
// canonical workspace's substrate), or evacuation was confirmed and the PVC
// delete request has already been issued.
func (r *WorkspaceReconciler) removeEvacuationFinalizer(ctx context.Context, ws *devplatformv1alpha1.Workspace) error {
	if !controllerutil.RemoveFinalizer(ws, evacuationFinalizer) {
		return nil
	}
	return r.Update(ctx, ws)
}

// evacuateBeforeStop captures ws's working directory off-node while the Pod
// that holds it is still running, recording the snapshot on ws.Status. It
// reports whether it is safe to stop compute.
//
// A workspace with no running compute has nothing left to capture, so it is
// reported safe rather than blocked: waiting on a Pod that will never answer
// would strand every already-suspended workspace at destroy time.
//
// ponytail: a workspace destroyed before it ever evacuated successfully keeps
// evacuationFinalizer, because StatusEvacuationConfirmer has no snapshot to
// confirm; an operator has to remove the finalizer by hand. Recording an
// explicit "nothing was ever written" marker on status is the upgrade path if
// that turns up in practice.
func (r *WorkspaceReconciler) evacuateBeforeStop(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string) (bool, error) {
	if resourceName == "" {
		return true, nil
	}
	running, err := r.computeRunning(ctx, ws.Namespace, resourceName)
	if err != nil || !running {
		return true, err
	}
	if r.EvacuationRequester == nil {
		return false, fmt.Errorf("devplatform: no EvacuationRequester configured")
	}

	snapshot, err := r.EvacuationRequester.RequestEvacuation(ctx, ws)
	if err != nil {
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:    conditionEvacuated,
			Status:  metav1.ConditionFalse,
			Reason:  "EvacuationFailed",
			Message: err.Error(),
		})
		r.notify(ctx, ws, EventEvacuationFailed, err.Error())
		return false, r.Status().Update(ctx, ws)
	}

	ws.Status.LastEvacuation = snapshot
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:    conditionEvacuated,
		Status:  metav1.ConditionTrue,
		Reason:  "SnapshotCaptured",
		Message: "working directory captured to " + snapshot.BundleKey,
	})
	// Persisted here rather than by the caller: once compute is torn down
	// nothing can be re-evacuated, so a retry that re-read the Workspace from
	// the API server must still find this snapshot.
	return true, r.Status().Update(ctx, ws)
}

// computeRunning reports whether the workspace's StatefulSet currently has a
// ready replica, which is the only state a supervisor can answer from.
func (r *WorkspaceReconciler) computeRunning(ctx context.Context, namespace, resourceName string) (bool, error) {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: namespace}, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return sts.Status.ReadyReplicas >= 1, nil
}
