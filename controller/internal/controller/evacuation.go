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

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// evacuationFinalizer holds a Workspace object around after Delete until
	// evacuation is confirmed, so reconcileTerminating gets a chance to gate
	// the PVC-delete step on it instead of the object simply vanishing.
	evacuationFinalizer = "devplatform.fickledev.com/evacuation"

	// evacuationPollInterval paces the Terminating requeue loop while
	// evacuation is still in flight.
	evacuationPollInterval = 10 * time.Second
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
