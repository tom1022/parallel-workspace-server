package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
	"github.com/tom1022/gitops-apps/apps/devplatform/controller/internal/adapter/database"
)

// backdateLastActivity rewrites ws's lastActivityAt directly via the status
// subresource, so idle-timeout tests never wait out D-2 in real time.
func backdateLastActivity(t *testing.T, ctx context.Context, req types.NamespacedName, when time.Time) {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	ts := metav1.NewTime(when)
	ws.Status.LastActivityAt = &ts
	if err := testClient.Status().Update(ctx, &ws); err != nil {
		t.Fatalf("backdate lastActivityAt: %v", err)
	}
}

// backdateSuspendedCondition rewrites the Suspended condition's
// LastTransitionTime, so the 7-day destroy-candidate check never waits out
// D-2's retention half in real time.
func backdateSuspendedCondition(t *testing.T, ctx context.Context, req types.NamespacedName, when time.Time) {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	found := false
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == conditionSuspended {
			ws.Status.Conditions[i].LastTransitionTime = metav1.NewTime(when)
			found = true
		}
	}
	if !found {
		t.Fatalf("Suspended condition not present on %s", req.Name)
	}
	if err := testClient.Status().Update(ctx, &ws); err != nil {
		t.Fatalf("backdate Suspended condition: %v", err)
	}
}

func markStatefulSetNotReady(t *testing.T, ctx context.Context, ns, name string) {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &sts); err != nil {
		t.Fatalf("get StatefulSet %s: %v", name, err)
	}
	sts.Status.ReadyReplicas = 0
	if err := testClient.Status().Update(ctx, &sts); err != nil {
		t.Fatalf("mark StatefulSet %s not ready: %v", name, err)
	}
}

func statefulSetReplicas(t *testing.T, ctx context.Context, ns, name string) int32 {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &sts); err != nil {
		t.Fatalf("get StatefulSet %s: %v", name, err)
	}
	if sts.Spec.Replicas == nil {
		return 1 // API default when unset
	}
	return *sts.Spec.Replicas
}

func TestReconcile_SuspendsAfterIdleTimeout(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-idle", "feature/idle")
	req := types.NamespacedName{Name: "ws-idle", Namespace: ns}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))

	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Fatalf("phase = %q, want Suspended", ws.Status.Phase)
	}
	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == conditionSuspended && c.Status == metav1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Error("expected a Suspended=True condition")
	}
	if got := statefulSetReplicas(t, ctx, ns, resourceName); got != 0 {
		t.Errorf("StatefulSet replicas = %d, want 0 (13.2: execution environment stopped)", got)
	}

	// 13.2/13.6: the working directory (PVC) must survive suspension untouched.
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc); err != nil {
		t.Fatalf("expected PVC to survive suspend: %v", err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		t.Error("PVC has a DeletionTimestamp after suspend; must be retained (13.2)")
	}
}

func TestReconcile_ResumesOnActivityAfterSuspend(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-resume", "feature/resume")
	req := types.NamespacedName{Name: "ws-resume", Namespace: ns}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))
	r := newTestReconciler()
	creq := ctrl.Request{NamespacedName: req}
	if _, err := r.Reconcile(ctx, creq); err != nil {
		t.Fatalf("reconcile (suspend): %v", err)
	}

	// envtest runs no real StatefulSet controller, so status.readyReplicas
	// does not drop automatically when spec.replicas is scaled to 0 the way a
	// real cluster's pod termination would; reset it explicitly so the resume
	// assertions below observe a genuine not-ready -> ready transition.
	markStatefulSetNotReady(t, ctx, ns, resourceName)

	var pvcBefore corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvcBefore); err != nil {
		t.Fatalf("get PVC before resume: %v", err)
	}

	// 13.3: a connection request or task submission against a Suspended
	// workspace is the resume trigger, delivered via MarkActivity.
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Fatalf("precondition: phase = %q, want Suspended", ws.Status.Phase)
	}
	if err := r.MarkActivity(ctx, &ws); err != nil {
		t.Fatalf("MarkActivity: %v", err)
	}

	// 13.5: readiness must be judged from the StatefulSet's current state, not
	// from history — envtest runs no kubelet, so the first resume pass must
	// stay Suspended (still resuming) until the Pod is marked ready.
	if _, err := r.Reconcile(ctx, creq); err != nil {
		t.Fatalf("reconcile (resume attempt 1): %v", err)
	}
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Fatalf("phase = %q, want still Suspended before the pod reports ready", ws.Status.Phase)
	}
	if got := statefulSetReplicas(t, ctx, ns, resourceName); got != 1 {
		t.Fatalf("StatefulSet replicas = %d, want 1 (13.3: reconnect to the retained working directory)", got)
	}

	markStatefulSetReady(t, ctx, ns, resourceName)
	if _, err := r.Reconcile(ctx, creq); err != nil {
		t.Fatalf("reconcile (resume attempt 2): %v", err)
	}
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready", ws.Status.Phase)
	}
	if ws.Status.SessionId == "" {
		t.Error("expected a re-created sessionId on resume (13.4)")
	}
	for _, c := range ws.Status.Conditions {
		if c.Type == conditionResumeRequested {
			t.Errorf("ResumeRequested condition should be cleared once resumed, got %+v", c)
		}
	}

	var pvcAfter corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvcAfter); err != nil {
		t.Fatalf("get PVC after resume: %v", err)
	}
	if pvcAfter.UID != pvcBefore.UID {
		t.Errorf("PVC UID changed across suspend/resume (%s -> %s); working directory must be reused, not recreated", pvcBefore.UID, pvcAfter.UID)
	}
}

func TestReconcile_FlagsDestroyCandidateAfterRetentionPeriod(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReady(t, ctx, ns, "ws-retention", "feature/retention")
	req := types.NamespacedName{Name: "ws-retention", Namespace: ns}

	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))
	r := newTestReconciler()
	creq := ctrl.Request{NamespacedName: req}
	if _, err := r.Reconcile(ctx, creq); err != nil {
		t.Fatalf("reconcile (suspend): %v", err)
	}

	backdateSuspendedCondition(t, ctx, req, time.Now().Add(-8*24*time.Hour))
	if _, err := r.Reconcile(ctx, creq); err != nil {
		t.Fatalf("reconcile (retention check): %v", err)
	}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseSuspended {
		t.Fatalf("phase = %q, want still Suspended (destroy-candidate is a notification, not a phase change)", ws.Status.Phase)
	}
	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == conditionDestroyCandidate && c.Status == metav1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Error("expected a DestroyCandidate=True condition after the retention period elapses (13.8)")
	}
}

// TestReconcile_TerminatingDeletesSubstrateAndSetsPhase covers destroy's
// compute/database half, which tears down regardless of evacuation status
// (13.7). The evacuation-gated PVC half — held while unconfirmed, deleted
// once confirmed — is evacuation_test.go's job; this test pins evacuation to
// "not yet confirmed" (fakeEvacuationConfirmer{complete: false}) precisely so
// the Workspace object survives the reconcile call for these assertions
// (task 2.1's own finalizer, added automatically by Reconcile, would
// otherwise let the object vanish the moment PVC deletion completes).
func TestReconcile_TerminatingDeletesSubstrateAndSetsPhase(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	resourceName := provisionToReady(t, ctx, ns, "ws-terminate", "feature/terminate")
	req := types.NamespacedName{Name: "ws-terminate", Namespace: ns}

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &ws); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}

	r := newTestReconciler()
	r.EvacuationConfirmer = &fakeEvacuationConfirmer{complete: false}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile (terminating): %v", err)
	}

	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace after reconcile: %v", err)
	}
	if ws.Status.Phase != devplatformv1alpha1.WorkspacePhaseTerminating {
		t.Fatalf("phase = %q, want Terminating", ws.Status.Phase)
	}
	if !controllerutil.ContainsFinalizer(&ws, evacuationFinalizer) {
		t.Error("expected evacuationFinalizer to still be present while evacuation is unconfirmed")
	}

	err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		var sts appsv1.StatefulSet
		return apierrors.IsNotFound(testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &sts)), nil
	})
	if err != nil {
		t.Fatalf("expected StatefulSet deleted after destroy request: %v", err)
	}

	errDB := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, database.CNPGDatabaseRef(ns, resourceName))
	if !apierrors.IsNotFound(errDB) {
		t.Errorf("expected Database deleted after destroy request, err=%v", errDB)
	}
	errRole := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, database.CNPGDatabaseRoleRef(ns, resourceName))
	if !apierrors.IsNotFound(errRole) {
		t.Errorf("expected DatabaseRole deleted after destroy request, err=%v", errRole)
	}

	// 16.4: the PVC itself must not be touched while evacuation is unconfirmed.
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc); err != nil {
		t.Fatalf("expected PVC to still exist while evacuation is unconfirmed: %v", err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		t.Error("expected PVC to have no deletion request while evacuation is unconfirmed")
	}

	// Cleanup: confirm evacuation and let the Workspace (and its namespace,
	// at test-run end) actually go away.
	r.EvacuationConfirmer = &fakeEvacuationConfirmer{complete: true}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile (terminating, cleanup): %v", err)
	}
}

// notifyRecorder captures what the reconciler would relay to Hermes Agent.
type notifyRecorder struct {
	kinds   []string
	details []string
}

func (n *notifyRecorder) record(_ context.Context, _ *devplatformv1alpha1.Workspace, kind, detail string) {
	n.kinds = append(n.kinds, kind)
	n.details = append(n.details, detail)
}

// createTemplateOnNode builds a template pinned to its own node, so a test that
// takes that node down does not disturb the Node object every other test in
// this package shares.
func createTemplateOnNode(t *testing.T, ctx context.Context, ns, name, nodeName string) {
	t.Helper()
	tmpl := &devplatformv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: devplatformv1alpha1.WorkspaceTemplateSpec{
			Image: "busybox:1.36",
			Resources: devplatformv1alpha1.WorkspaceResources{
				Requests: devplatformv1alpha1.ResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   devplatformv1alpha1.ResourceList{CPU: "500m", Memory: "256Mi"},
			},
			Storage:    devplatformv1alpha1.WorkspaceStorage{Size: "1Gi"},
			NodeName:   nodeName,
			Database:   &devplatformv1alpha1.WorkspaceDatabaseRef{ClusterRef: "devplatform-db"},
			Auth:       devplatformv1alpha1.WorkspaceAuthRef{SecretRef: "claude-auth"},
			Evacuation: devplatformv1alpha1.WorkspaceEvacuation{Bucket: "workspace", Endpoint: "http://garage.garage.svc.cluster.local:3900", Region: "garage", SecretRef: "garage-evacuation-credentials"},
		},
	}
	if err := testClient.Create(ctx, tmpl); err != nil {
		t.Fatalf("create WorkspaceTemplate: %v", err)
	}
	ensureStagedNode(t, ctx, nodeName, tmpl.Spec.Image)
}

func setNodeReady(t *testing.T, ctx context.Context, nodeName string, ready bool) {
	t.Helper()
	var node corev1.Node
	if err := testClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("get Node %s: %v", nodeName, err)
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node.Status.Conditions = []corev1.NodeCondition{{
		Type:              corev1.NodeReady,
		Status:            status,
		LastHeartbeatTime: metav1.Now(),
	}}
	if err := testClient.Status().Update(ctx, &node); err != nil {
		t.Fatalf("set Node %s ready=%v: %v", nodeName, ready, err)
	}
}

// suspendToNode drives a workspace on its own node all the way to Suspended.
func suspendToNode(t *testing.T, ctx context.Context, ns, name, templateRef string) types.NamespacedName {
	t.Helper()
	createWorkspace(t, ctx, ns, name, "https://gitea.fickledev.com/tom1022/demo.git", "feature/"+name, templateRef)
	req := types.NamespacedName{Name: name, Namespace: ns}

	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	markStatefulSetReady(t, ctx, ns, ws.Status.WorkspaceId)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	backdateLastActivity(t, ctx, req, time.Now().Add(-31*time.Minute))
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("suspend reconcile: %v", err)
	}
	return req
}

func TestReconcile_NotifiesOnceThatALostNodeBlocksResumeWithWorkEvacuated(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplateOnNode(t, ctx, ns, "own-node", "node-lost-evacuated")
	req := suspendToNode(t, ctx, ns, "ws-node-evacuated", "own-node")

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	captured := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	ws.Status.LastEvacuation = &devplatformv1alpha1.EvacuationSnapshot{
		WorkspaceId: ws.Status.WorkspaceId,
		BundleKey:   "workspace/" + ws.Status.WorkspaceId + "/latest/bundle.git",
		CapturedAt:  &captured,
	}
	if err := testClient.Status().Update(ctx, &ws); err != nil {
		t.Fatalf("record snapshot: %v", err)
	}

	setNodeReady(t, ctx, "node-lost-evacuated", false)

	rec := &notifyRecorder{}
	r := newTestReconciler()
	r.Notify = rec.record
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.kinds) != 1 || rec.kinds[0] != EventNodeUnavailable {
		t.Fatalf("kinds = %v, want one %s", rec.kinds, EventNodeUnavailable)
	}
	detail := rec.details[0]
	for _, want := range []string{"node-lost-evacuated", captured.UTC().Format(time.RFC3339)} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}

	// 13.9 asks for one notification about a standing condition, not one per
	// reconcile pass.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(rec.kinds) != 1 {
		t.Errorf("notified %d times, want 1", len(rec.kinds))
	}
}

// The two outcomes 13.9 has to keep apart: the node is gone but the work is
// off-node, versus the node is gone and nothing was ever captured.
func TestReconcile_LostNodeNotificationSaysWhenNothingWasEvacuated(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplateOnNode(t, ctx, ns, "own-node", "node-lost-unevacuated")
	req := suspendToNode(t, ctx, ns, "ws-node-unevacuated", "own-node")

	// Suspending evacuates first, so the never-evacuated state — a node lost
	// while the workspace was still Ready, or an evacuation that never
	// succeeded — has to be restored explicitly here.
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	ws.Status.LastEvacuation = nil
	if err := testClient.Status().Update(ctx, &ws); err != nil {
		t.Fatalf("clear snapshot: %v", err)
	}

	setNodeReady(t, ctx, "node-lost-unevacuated", false)

	rec := &notifyRecorder{}
	r := newTestReconciler()
	r.Notify = rec.record
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.details) != 1 {
		t.Fatalf("notified %d times, want 1", len(rec.details))
	}
	if !strings.Contains(rec.details[0], "no evacuation snapshot") {
		t.Errorf("detail %q does not distinguish unevacuated work from a recoverable one", rec.details[0])
	}
}

// A node can also die while the workspace is still Ready; its Pod cannot be
// rescheduled because the working directory is pinned to that node.
func TestReconcile_NotifiesWhenAReadyWorkspaceLosesItsNode(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplateOnNode(t, ctx, ns, "own-node", "node-lost-while-ready")
	createWorkspace(t, ctx, ns, "ws-node-ready", "https://gitea.fickledev.com/tom1022/demo.git", "feature/ready", "own-node")
	req := types.NamespacedName{Name: "ws-node-ready", Namespace: ns}

	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, req, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	markStatefulSetReady(t, ctx, ns, ws.Status.WorkspaceId)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	setNodeReady(t, ctx, "node-lost-while-ready", false)

	rec := &notifyRecorder{}
	r.Notify = rec.record
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: req}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.kinds) != 1 || rec.kinds[0] != EventNodeUnavailable {
		t.Fatalf("kinds = %v, want one %s", rec.kinds, EventNodeUnavailable)
	}
}

// 1.6/13.7: a workspace that only mirrors another's substrate owns none of it,
// so destroying it must leave the canonical workspace running.
func TestReconcile_DestroyingAMirrorLeavesTheCanonicalSubstrate(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	branch := "feature/mirror-destroy"
	resourceName := provisionToReady(t, ctx, ns, "ws-canonical", branch)

	createWorkspace(t, ctx, ns, "ws-mirror", "https://gitea.fickledev.com/tom1022/demo.git", branch, "default")
	mirrorReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-mirror", Namespace: ns}}
	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, mirrorReq); err != nil {
		t.Fatalf("reconcile ws-mirror: %v", err)
	}

	var mirror devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, mirrorReq.NamespacedName, &mirror); err != nil {
		t.Fatalf("get ws-mirror: %v", err)
	}
	if !meta.IsStatusConditionTrue(mirror.Status.Conditions, conditionDuplicateOfExisting) {
		t.Fatalf("precondition: ws-mirror is not marked a duplicate: %+v", mirror.Status.Conditions)
	}
	if mirror.Status.WorkspaceId != resourceName {
		t.Fatalf("precondition: ws-mirror workspaceId = %q, want the canonical's %q", mirror.Status.WorkspaceId, resourceName)
	}

	if err := testClient.Delete(ctx, &mirror); err != nil {
		t.Fatalf("delete ws-mirror: %v", err)
	}
	if _, err := r.Reconcile(ctx, mirrorReq); err != nil {
		t.Fatalf("reconcile ws-mirror (terminating): %v", err)
	}

	var sts appsv1.StatefulSet
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &sts); err != nil {
		t.Fatalf("canonical StatefulSet must survive the mirror's destruction: %v", err)
	}
	if !sts.DeletionTimestamp.IsZero() {
		t.Error("canonical StatefulSet has a DeletionTimestamp after the mirror was destroyed")
	}
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: ns}, &pvc); err != nil {
		t.Fatalf("canonical PVC must survive the mirror's destruction: %v", err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		t.Error("canonical PVC has a DeletionTimestamp after the mirror was destroyed")
	}

	var canonical devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, types.NamespacedName{Name: "ws-canonical", Namespace: ns}, &canonical); err != nil {
		t.Fatalf("get ws-canonical: %v", err)
	}
	if canonical.Status.Phase != devplatformv1alpha1.WorkspacePhaseReady {
		t.Errorf("canonical phase = %q, want it still Ready", canonical.Status.Phase)
	}
}
