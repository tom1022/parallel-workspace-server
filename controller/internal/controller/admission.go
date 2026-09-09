// This file covers task 2.5: admission before provisioning (design.md
// "Workspace Controller" Implementation Notes / Validation, Requirement
// 15.4/15.6/15.9/15.11). It runs before reconcileDatabase (2.4's hand-off
// note) so a workspace held for resource limits never leaves partial
// incidental substrate to roll back. Three gates, checked in order and each
// short-circuiting on the first failure:
//
//  1. the target node has the template's base image pre-staged (15.4)
//  2. the namespace's ResourceQuota has room for one more workspace Pod (15.6)
//  3. the target node's declared working-directory disk budget has room (15.11)
//
// None of these fail the Workspace. They leave it in Provisioning with a
// ResourceWaiting condition and requeue, indefinitely — unlike
// requeueOrFail's D-1 timeout, resource waits are expected to outlive a
// single provisioning attempt until capacity frees up elsewhere.
package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// conditionResourceWaiting communicates a hold for capacity to the
	// requester (15.6/15.11), distinct from conditionProvisioned which tracks
	// substrate creation itself.
	conditionResourceWaiting = "ResourceWaiting"

	// workspaceResourceQuotaName is the ResourceQuota apps/devplatform/templates/
	// workspace-quota.yaml applies to the workspace namespace (15.5). Absent
	// (quota not deployed) is treated as "nothing to enforce", not an error.
	workspaceResourceQuotaName = "devplatform-workspace-quota"
)

// reconcileAdmission returns a non-nil Result when ws must stay in
// Provisioning and be requeued for a resource limit; nil, nil means the
// caller may proceed with provisioning.
func (r *WorkspaceReconciler) reconcileAdmission(ctx context.Context, ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate) (*ctrl.Result, error) {
	blocked, reason, message, err := r.admissionBlocked(ctx, ws, tmpl)
	if err != nil {
		return nil, err
	}

	if blocked {
		log.FromContext(ctx).Info("workspace held for admission",
			"workspace", ws.Name, "namespace", ws.Namespace, "reason", reason, "message", message)
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:    conditionResourceWaiting,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})
		if err := r.Status().Update(ctx, ws); err != nil {
			return nil, err
		}
		result := ctrl.Result{RequeueAfter: r.pollInterval()}
		return &result, nil
	}

	if cond := meta.FindStatusCondition(ws.Status.Conditions, conditionResourceWaiting); cond != nil && cond.Status == metav1.ConditionTrue {
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:    conditionResourceWaiting,
			Status:  metav1.ConditionFalse,
			Reason:  "AdmissionGranted",
			Message: "resource limits are no longer exceeded",
		})
		if err := r.Status().Update(ctx, ws); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (r *WorkspaceReconciler) admissionBlocked(ctx context.Context, ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate) (bool, string, string, error) {
	staged, err := r.imageStagedOnNode(ctx, tmpl.Spec.NodeName, tmpl.Spec.Image)
	if err != nil {
		return false, "", "", err
	}
	if !staged {
		return true, "ImageNotStaged", fmt.Sprintf("base image %q is not yet pre-staged on node %q", tmpl.Spec.Image, tmpl.Spec.NodeName), nil
	}

	quotaOK, quotaMessage, err := r.resourceQuotaHasRoom(ctx, ws, tmpl)
	if err != nil {
		return false, "", "", err
	}
	if !quotaOK {
		return true, "QuotaExceeded", quotaMessage, nil
	}

	diskOK, diskMessage, err := r.nodeDiskBudgetHasRoom(ctx, ws, tmpl)
	if err != nil {
		return false, "", "", err
	}
	if !diskOK {
		return true, "QuotaExceeded", diskMessage, nil // ProvisionError{kind: QuotaExceeded, resource: "disk"}
	}

	return false, "", "", nil
}

// imageStagedOnNode judges 15.4 the way image-prestage-daemonset.yaml's own
// comments specify: a Pod is Running on the node only once kubelet has
// pulled its image, so the node's own advertised image cache
// (Node.status.images, standard Kubernetes API field) is the pre-stage
// signal — no extra propagation mechanism (e.g. a Node label) is needed.
//
// A template image is often a tag+digest reference (repo:tag@sha256:...),
// but kubelet records the pulled image under its digest-only form
// (repo@sha256:...) once it resolves the tag, so an exact string match
// against Node.status.images never fires for that (common, real-cluster)
// shape. Falling back to a digest-suffix comparison covers it without
// losing the exact match for plain tag references.
func (r *WorkspaceReconciler) imageStagedOnNode(ctx context.Context, nodeName, image string) (bool, error) {
	var node corev1.Node
	if err := r.nodeReader().Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	digest := imageDigestSuffix(image)
	for _, img := range node.Status.Images {
		for _, name := range img.Names {
			if name == image {
				return true, nil
			}
			if digest != "" && strings.HasSuffix(name, digest) {
				return true, nil
			}
		}
	}
	return false, nil
}

// imageDigestSuffix returns the "@sha256:..." suffix of an image reference,
// or "" if it carries no digest.
func imageDigestSuffix(image string) string {
	if i := strings.Index(image, "@sha256:"); i >= 0 {
		return image[i:]
	}
	return ""
}

// resourceQuotaHasRoom checks 15.6 against the namespace ResourceQuota's
// already-computed Status (Hard/Used), rather than listing Pods and summing
// requests itself — the resourcequota controller already owns that
// accounting, so this only asks whether headroom exists for one more
// workspace Pod at ws's template's requests/limits.
func (r *WorkspaceReconciler) resourceQuotaHasRoom(ctx context.Context, ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate) (bool, string, error) {
	var rq corev1.ResourceQuota
	err := r.Get(ctx, types.NamespacedName{Name: workspaceResourceQuotaName, Namespace: ws.Namespace}, &rq)
	if apierrors.IsNotFound(err) {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}

	requests, err := parseResourceList(tmpl.Spec.Resources.Requests)
	if err != nil {
		return false, "", err
	}
	limits, err := parseResourceList(tmpl.Spec.Resources.Limits)
	if err != nil {
		return false, "", err
	}

	checks := []struct {
		name     corev1.ResourceName
		needed   resource.Quantity
		resource string // ProvisionError.resource vocabulary (design.md Service Interface)
	}{
		{corev1.ResourcePods, resource.MustParse("1"), "pods"},
		{corev1.ResourceName("requests.cpu"), requests[corev1.ResourceCPU], "cpu"},
		{corev1.ResourceName("requests.memory"), requests[corev1.ResourceMemory], "memory"},
		{corev1.ResourceName("limits.cpu"), limits[corev1.ResourceCPU], "cpu"},
		{corev1.ResourceName("limits.memory"), limits[corev1.ResourceMemory], "memory"},
	}
	for _, c := range checks {
		hard, hasHard := rq.Status.Hard[c.name]
		if !hasHard {
			continue // not governed by this quota: nothing to enforce for it
		}
		used := rq.Status.Used[c.name] // zero value is a valid "0" Quantity
		remaining := hard.DeepCopy()
		remaining.Sub(used)
		if remaining.Cmp(c.needed) < 0 {
			return false, fmt.Sprintf("ResourceQuota %q has no remaining %s for a new workspace", workspaceResourceQuotaName, c.name), nil
		}
	}
	return true, "", nil
}

// nodeDiskBudgetHasRoom approximates 15.11. Actual disk usage isn't
// observable from the Kubernetes API alone (kubelet stats / metrics-server
// are out of scope here); instead this sums the declared Storage.Size of
// every other non-Failed, non-duplicate Workspace whose template targets the
// same node, plus ws's own request, against
// tmpl.Spec.Storage.NodeDiskBudget. Duplicates (conditionDuplicateOfExisting)
// are excluded: they mirror another Workspace's status and never get their
// own PVC (1.6), so counting them would double-count that branch's storage.
func (r *WorkspaceReconciler) nodeDiskBudgetHasRoom(ctx context.Context, ws *devplatformv1alpha1.Workspace, tmpl *devplatformv1alpha1.WorkspaceTemplate) (bool, string, error) {
	if tmpl.Spec.Storage.NodeDiskBudget == "" {
		return true, "", nil
	}
	budget, err := resource.ParseQuantity(tmpl.Spec.Storage.NodeDiskBudget)
	if err != nil {
		return false, "", fmt.Errorf("devplatform: invalid nodeDiskBudget %q on template %q: %w", tmpl.Spec.Storage.NodeDiskBudget, tmpl.Name, err)
	}

	total, err := storageQuantity(tmpl.Spec.Storage.Size)
	if err != nil {
		return false, "", err
	}

	var list devplatformv1alpha1.WorkspaceList
	if err := r.List(ctx, &list, client.InNamespace(ws.Namespace)); err != nil {
		return false, "", err
	}
	for _, other := range list.Items {
		if other.Name == ws.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Status.Phase == devplatformv1alpha1.WorkspacePhaseFailed {
			continue // rolled back already; no PVC left to count (failAndRollback)
		}
		if meta.IsStatusConditionTrue(other.Status.Conditions, conditionDuplicateOfExisting) {
			continue
		}

		var otherTmpl devplatformv1alpha1.WorkspaceTemplate
		if err := r.Get(ctx, types.NamespacedName{Name: other.Spec.TemplateRef, Namespace: other.Namespace}, &otherTmpl); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, "", err
		}
		if otherTmpl.Spec.NodeName != tmpl.Spec.NodeName {
			continue
		}

		otherSize, err := storageQuantity(otherTmpl.Spec.Storage.Size)
		if err != nil {
			return false, "", err
		}
		total.Add(otherSize)
	}

	if total.Cmp(budget) > 0 {
		return false, fmt.Sprintf("node %q working-directory disk budget %s would be exceeded (requested total %s)", tmpl.Spec.NodeName, budget.String(), total.String()), nil
	}
	return true, "", nil
}

func storageQuantity(size string) (resource.Quantity, error) {
	if size == "" {
		size = defaultWorkspaceStorageSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("devplatform: invalid storage size %q: %w", size, err)
	}
	return qty, nil
}
