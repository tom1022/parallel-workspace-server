// Package controller implements the Workspace Controller reconciliation
// loops. This file covers task 2.1 only: the provisioning state machine
// (Provisioning -> Ready | Failed) for a Workspace's PVC + StatefulSet
// substrate. Database (2.2), routing (2.3), git credentials (2.4), resource
// admission (2.5) and the Suspended/Terminating lifecycle (2.6/2.7) are
// separate sub-reconcilers layered on top of reconcileProvisioning /
// reconcileReady / reconcileSuspended / reconcileFailed below.
package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// defaultProvisioningTimeout is D-1: the upper bound from request receipt to
	// Ready (design.md §Performance & Scalability).
	defaultProvisioningTimeout = 180 * time.Second
	defaultPollInterval        = 5 * time.Second

	conditionProvisioned = "Provisioned"

	// conditionDuplicateOfExisting marks a Workspace that mirrors another
	// canonical Workspace's substrate instead of owning its own (1.6). Shared
	// with admission.go's disk-budget approximation: a duplicate never gets
	// its own PVC, so it must not be counted twice toward a node's budget.
	conditionDuplicateOfExisting = "DuplicateOfExisting"
)

// WorkspaceReconciler reconciles a Workspace object.
type WorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ProvisioningTimeout overrides defaultProvisioningTimeout; tests set this
	// to a small value instead of waiting out D-1 in real time.
	ProvisioningTimeout time.Duration
	// PollInterval overrides defaultPollInterval.
	PollInterval time.Duration
	// Now overrides time.Now for elapsed-time checks; tests may fake it.
	Now func() time.Time

	// GitCredentialIssuer issues the repository-scoped Git credential each
	// workspace connects to its remote with (task 2.4). No default: a nil
	// value fails provisioning explicitly rather than silently skipping
	// credential issuance.
	GitCredentialIssuer GitCredentialIssuer

	// NodeReader reads Node objects directly against the API server instead
	// of through the manager's cache (task 2.5). The manager cache is scoped
	// to the workspace namespace (main.go), and Node is cluster-scoped, so a
	// dedicated uncached reader is needed for the image pre-stage check
	// (design.md: judge 15.4 from Node.status.images). Falls back to Client
	// when unset, which is fine for the uncached client tests use.
	NodeReader client.Reader

	// EvacuationConfirmer confirms off-node evacuation completion before a
	// destroyed workspace's PVC is actually deleted (task 2.7, 16.4). No
	// default: reconcileTerminating fails explicitly rather than deleting a
	// PVC it never confirmed was evacuated (see evacuation.go).
	EvacuationConfirmer EvacuationConfirmer

	// EvacuationRequester asks the workspace's Session Supervisor to capture
	// the working directory before compute is stopped or torn down (16.8). No
	// default, for the same reason as EvacuationConfirmer: a missing requester
	// must not read as "nothing to evacuate".
	EvacuationRequester EvacuationRequester

	// Notify reports a condition a human has to act on. Unset is a no-op:
	// chat relay is the platform's only notification path and losing it must
	// not take reconciliation down with it.
	Notify func(ctx context.Context, ws *devplatformv1alpha1.Workspace, kind, detail string)
}

func (r *WorkspaceReconciler) notify(ctx context.Context, ws *devplatformv1alpha1.Workspace, kind, detail string) {
	if r.Notify == nil {
		return
	}
	r.Notify(ctx, ws, kind, detail)
}

func (r *WorkspaceReconciler) nodeReader() client.Reader {
	if r.NodeReader != nil {
		return r.NodeReader
	}
	return r.Client
}

func (r *WorkspaceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *WorkspaceReconciler) timeout() time.Duration {
	if r.ProvisioningTimeout > 0 {
		return r.ProvisioningTimeout
	}
	return defaultProvisioningTimeout
}

func (r *WorkspaceReconciler) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return defaultPollInterval
}

func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ws devplatformv1alpha1.Workspace
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ws.DeletionTimestamp.IsZero() {
		// Explicit deletes (lifecycle.go's reconcileTerminating) rather than
		// relying solely on OwnerReference GC, so the evacuation-wait
		// Finalizer added below can hold ws around without also holding its
		// compute (StatefulSet) around (13.7).
		return r.reconcileTerminating(ctx, &ws)
	}

	// Registered on every non-deleting pass (not just Provisioning's first
	// one) so a workspace that somehow reaches Ready/Suspended without it —
	// e.g. an object created before this field existed — still gets it
	// before it can ever be deleted without the evacuation-wait gate (16.4).
	if controllerutil.AddFinalizer(&ws, evacuationFinalizer) {
		if err := r.Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch ws.Status.Phase {
	case devplatformv1alpha1.WorkspacePhaseReady:
		return r.reconcileReady(ctx, &ws)
	case devplatformv1alpha1.WorkspacePhaseSuspended:
		return r.reconcileSuspended(ctx, &ws)
	case devplatformv1alpha1.WorkspacePhaseFailed:
		return r.reconcileFailed(ctx, &ws)
	default: // "" and Provisioning
		return r.reconcileProvisioning(ctx, &ws)
	}
}

// reconcileFailed is a placeholder extension point; 2.1 does not retry from
// Failed automatically.
func (r *WorkspaceReconciler) reconcileFailed(_ context.Context, _ *devplatformv1alpha1.Workspace) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *WorkspaceReconciler) reconcileProvisioning(ctx context.Context, ws *devplatformv1alpha1.Workspace) (ctrl.Result, error) {
	// 1.6: an existing workspace for the same repository+branch wins; this one
	// mirrors its status instead of provisioning duplicate substrate.
	canonical, err := r.findCanonical(ctx, ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	if canonical != nil {
		return r.mirrorCanonical(ctx, ws, canonical)
	}

	tmpl, err := r.getTemplate(ctx, ws)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.requeueOrFail(ctx, ws, "", "WorkspaceTemplateNotFound", err.Error())
		}
		return ctrl.Result{}, err
	}

	// 2.5: admission runs before any incidental resource (including the
	// database) is created, so a workspace held for resource limits leaves no
	// partial substrate behind to roll back.
	if pending, err := r.reconcileAdmission(ctx, ws, tmpl); err != nil {
		return ctrl.Result{}, err
	} else if pending != nil {
		return *pending, nil
	}

	resourceName, err := r.resolveResourceName(ctx, ws)
	if err != nil {
		return r.failAndRollback(ctx, ws, "", "NameDerivationFailed", err.Error())
	}
	if ws.Status.WorkspaceId != resourceName {
		ws.Status.WorkspaceId = resourceName
		if err := r.Status().Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.reconcileDatabase(ctx, ws, tmpl, resourceName); err != nil {
		return r.requeueOrFail(ctx, ws, resourceName, "DatabaseUnavailable", err.Error())
	}

	pvc, err := buildPVC(ws, tmpl, resourceName)
	if err != nil {
		return r.failAndRollback(ctx, ws, resourceName, "InvalidTemplate", err.Error())
	}
	if err := controllerutil.SetControllerReference(ws, pvc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureCreated(ctx, pvc); err != nil {
		return r.requeueOrFail(ctx, ws, resourceName, "PVCCreateFailed", err.Error())
	}

	sts, err := buildStatefulSet(ws, tmpl, resourceName)
	if err != nil {
		return r.failAndRollback(ctx, ws, resourceName, "InvalidTemplate", err.Error())
	}
	if err := controllerutil.SetControllerReference(ws, sts, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureCreated(ctx, sts); err != nil {
		return r.requeueOrFail(ctx, ws, resourceName, "StatefulSetCreateFailed", err.Error())
	}

	var current appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ws.Namespace}, &current); err != nil {
		return ctrl.Result{}, err
	}
	if current.Status.ReadyReplicas < 1 {
		return r.requeueOrFail(ctx, ws, resourceName, "WaitingForStatefulSetReady", "waiting for the workspace pod to become ready")
	}

	urls, err := r.reconcileIngress(ctx, ws, resourceName)
	if err != nil {
		return r.requeueOrFail(ctx, ws, resourceName, "IngressUnavailable", err.Error())
	}

	if err := r.reconcileGitCredential(ctx, ws, resourceName); err != nil {
		return r.requeueOrFail(ctx, ws, resourceName, "GitCredentialUnavailable", err.Error())
	}

	return ctrl.Result{}, r.markReady(ctx, ws, resourceName, urls)
}

// findCanonical returns the pre-existing Workspace that owns repository+branch,
// if ws is not itself that owner. Ownership is decided deterministically by
// creation time (ties broken by name) so every reconciler run agrees on the
// same canonical object regardless of reconcile order.
func (r *WorkspaceReconciler) findCanonical(ctx context.Context, ws *devplatformv1alpha1.Workspace) (*devplatformv1alpha1.Workspace, error) {
	var list devplatformv1alpha1.WorkspaceList
	if err := r.List(ctx, &list, client.InNamespace(ws.Namespace)); err != nil {
		return nil, err
	}

	var candidates []devplatformv1alpha1.Workspace
	for _, item := range list.Items {
		if item.Name == ws.Name {
			continue
		}
		if item.Spec.Repository != ws.Spec.Repository || item.Spec.Branch != ws.Spec.Branch {
			continue
		}
		if !item.DeletionTimestamp.IsZero() {
			continue
		}
		candidates = append(candidates, item)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		ti, tj := candidates[i].CreationTimestamp, candidates[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return candidates[i].Name < candidates[j].Name
	})
	oldest := candidates[0]

	if isOlder(ws.CreationTimestamp, ws.Name, oldest.CreationTimestamp, oldest.Name) {
		return nil, nil // ws itself is the canonical owner
	}
	return &oldest, nil
}

func isOlder(t1 metav1.Time, name1 string, t2 metav1.Time, name2 string) bool {
	if !t1.Equal(&t2) {
		return t1.Before(&t2)
	}
	return name1 < name2
}

func (r *WorkspaceReconciler) mirrorCanonical(ctx context.Context, ws, canonical *devplatformv1alpha1.Workspace) (ctrl.Result, error) {
	ws.Status.Phase = canonical.Status.Phase
	if ws.Status.Phase == "" {
		ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseProvisioning
	}
	ws.Status.WorkspaceId = canonical.Status.WorkspaceId
	ws.Status.Urls = canonical.Status.Urls
	ws.Status.SessionId = canonical.Status.SessionId
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:    conditionDuplicateOfExisting,
		Status:  metav1.ConditionTrue,
		Reason:  "BranchAlreadyProvisioned",
		Message: fmt.Sprintf("repository %q branch %q is already provisioned by workspace %q", ws.Spec.Repository, ws.Spec.Branch, canonical.Name),
	})
	if err := r.Status().Update(ctx, ws); err != nil {
		return ctrl.Result{}, err
	}
	if canonical.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
	return ctrl.Result{}, nil
}

func (r *WorkspaceReconciler) getTemplate(ctx context.Context, ws *devplatformv1alpha1.Workspace) (*devplatformv1alpha1.WorkspaceTemplate, error) {
	var tmpl devplatformv1alpha1.WorkspaceTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: ws.Spec.TemplateRef, Namespace: ws.Namespace}, &tmpl); err != nil {
		return nil, err
	}
	return &tmpl, nil
}

// resolveResourceName is stable once assigned (reused from status), so
// substrate names never change across reconcile passes for the same
// Workspace.
func (r *WorkspaceReconciler) resolveResourceName(ctx context.Context, ws *devplatformv1alpha1.Workspace) (string, error) {
	if ws.Status.WorkspaceId != "" {
		return ws.Status.WorkspaceId, nil
	}

	var list devplatformv1alpha1.WorkspaceList
	if err := r.List(ctx, &list, client.InNamespace(ws.Namespace)); err != nil {
		return "", err
	}
	taken := func(name string) bool {
		for _, item := range list.Items {
			if item.Name == ws.Name {
				continue
			}
			if item.Status.WorkspaceId == name {
				return true
			}
		}
		return false
	}
	return DeriveResourceName(ws.Spec.Branch, taken)
}

func (r *WorkspaceReconciler) ensureCreated(ctx context.Context, obj client.Object) error {
	if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// requeueOrFail keeps ws in Provisioning and retries after pollInterval,
// unless D-1 has already elapsed since the workspace was requested, in which
// case it fails and rolls back (1.8/1.9).
func (r *WorkspaceReconciler) requeueOrFail(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName, reason, message string) (ctrl.Result, error) {
	if r.now().Sub(ws.CreationTimestamp.Time) < r.timeout() {
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
	return r.failAndRollback(ctx, ws, resourceName, "ProvisioningTimeout", fmt.Sprintf("exceeded provisioning timeout (last error: %s: %s)", reason, message))
}

// failAndRollback deletes any substrate generated so far and transitions the
// workspace to Failed with the reason recorded in status.conditions (1.8).
func (r *WorkspaceReconciler) failAndRollback(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName, reason, message string) (ctrl.Result, error) {
	if resourceName != "" {
		if err := client.IgnoreNotFound(r.Delete(ctx, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ws.Namespace},
		})); err != nil {
			return ctrl.Result{}, err
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ws.Namespace},
		})); err != nil {
			return ctrl.Result{}, err
		}
		// Database/DatabaseRole are only ever OwnerReference-GC'd on actual
		// object deletion (8.9/8.10); a rollback to Failed leaves ws alive, so
		// any partially-created substrate needs the same explicit cleanup the
		// StatefulSet/PVC above get.
		if err := client.IgnoreNotFound(r.Delete(ctx, databaseRef(ws.Namespace, resourceName))); err != nil {
			return ctrl.Result{}, err
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, databaseRoleRef(ws.Namespace, resourceName))); err != nil {
			return ctrl.Result{}, err
		}
	}

	ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseFailed
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:    conditionProvisioned,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	return ctrl.Result{}, r.Status().Update(ctx, ws)
}

func (r *WorkspaceReconciler) markReady(ctx context.Context, ws *devplatformv1alpha1.Workspace, resourceName string, urls devplatformv1alpha1.WorkspaceURLs) error {
	ws.Status.Phase = devplatformv1alpha1.WorkspacePhaseReady
	ws.Status.WorkspaceId = resourceName
	ws.Status.Urls = urls
	ws.Status.SessionId = resourceName + "-session"
	now := metav1.NewTime(r.now())
	ws.Status.LastActivityAt = &now
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:    conditionProvisioned,
		Status:  metav1.ConditionTrue,
		Reason:  "SubstrateReady",
		Message: "workspace PVC and StatefulSet are ready",
	})
	return r.Status().Update(ctx, ws)
}

// SetupWithManager wires the reconciler into a controller-runtime Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devplatformv1alpha1.Workspace{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}
