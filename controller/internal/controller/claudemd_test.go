package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

func claudeMDOf(t *testing.T, ctx context.Context, ns, workspaceName string) string {
	t.Helper()
	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, types.NamespacedName{Name: workspaceName, Namespace: ns}, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	var cm corev1.ConfigMap
	name := ClaudeMDConfigMapName(ws.Status.WorkspaceId)
	if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &cm); err != nil {
		t.Fatalf("get ConfigMap %s: %v", name, err)
	}
	return cm.Data[ClaudeMDKey]
}

// 10.4: the document generated for a workspace carries the other active
// branches' summaries and public interfaces, which is the whole point of
// handing it to the session.
func TestReconcile_ClaudeMDCarriesOtherBranches(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-md-a", "feature/md-a", "認証基盤の刷新", "AuthService.Login")
	provisionToReadyWithSummary(t, ctx, ns, "ws-md-b", "feature/md-b", "課金の見直し", "BillingService.Charge")

	// A's document only learns about B once A reconciles again with B's entry
	// already recorded.
	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-md-a", Namespace: ns}}); err != nil {
		t.Fatalf("reconcile ws-md-a: %v", err)
	}

	doc := claudeMDOf(t, ctx, ns, "ws-md-a")
	for _, want := range []string{"feature/md-b", "課金の見直し", "BillingService.Charge"} {
		if !strings.Contains(doc, want) {
			t.Errorf("CLAUDE.md does not mention %q:\n%s", want, doc)
		}
	}
}

// 10.5: the shared prefix has to be identical byte for byte across
// workspaces — that is what lets the prompt cache be reused — and only the
// branch-specific tail may differ.
func TestReconcile_ClaudeMDCommonPartIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-md-x", "feature/md-x", "検索の改善", "SearchService.Query")
	provisionToReadyWithSummary(t, ctx, ns, "ws-md-y", "feature/md-y", "通知の整理", "NotifyService.Send")

	// Both reconciled once more so each document is rendered from the same
	// set of active entries.
	r := newTestReconciler()
	for _, name := range []string{"ws-md-x", "ws-md-y"} {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	docX := claudeMDOf(t, ctx, ns, "ws-md-x")
	docY := claudeMDOf(t, ctx, ns, "ws-md-y")

	commonX, branchX, okX := strings.Cut(docX, ClaudeMDBranchMarker)
	commonY, branchY, okY := strings.Cut(docY, ClaudeMDBranchMarker)
	if !okX || !okY {
		t.Fatalf("both documents must carry the branch marker; x=%v y=%v", okX, okY)
	}
	if commonX != commonY {
		t.Errorf("common parts differ:\n--- x ---\n%s\n--- y ---\n%s", commonX, commonY)
	}
	if branchX == branchY {
		t.Errorf("branch-specific parts are identical, want each workspace's own branch: %q", branchX)
	}
	if !strings.Contains(branchX, "feature/md-x") {
		t.Errorf("branch-specific part = %q, want it to name feature/md-x", branchX)
	}
	if strings.Index(docX, ClaudeMDBranchMarker) == 0 {
		t.Error("branch-specific part precedes the common part, want it after")
	}
}

// 10.8/10.4: a destroyed branch stops appearing in the documents the surviving
// workspaces are handed.
func TestReconcile_ClaudeMDDropsDestroyedBranches(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-md-keep", "feature/md-keep", "残る作業", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-md-gone", "feature/md-gone", "消える作業", "")

	var gone devplatformv1alpha1.Workspace
	goneReq := types.NamespacedName{Name: "ws-md-gone", Namespace: ns}
	if err := testClient.Get(ctx, goneReq, &gone); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	if err := testClient.Delete(ctx, &gone); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}
	r := newTestReconciler()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: goneReq}); err != nil {
		t.Fatalf("reconcile destroyed workspace: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-md-keep", Namespace: ns}}); err != nil {
		t.Fatalf("reconcile surviving workspace: %v", err)
	}

	doc := claudeMDOf(t, ctx, ns, "ws-md-keep")
	if strings.Contains(doc, "feature/md-gone") {
		t.Errorf("CLAUDE.md still lists the destroyed branch:\n%s", doc)
	}
	if !strings.Contains(doc, "feature/md-keep") {
		t.Errorf("CLAUDE.md lost the surviving branch:\n%s", doc)
	}
}

// 10.9: the document reaches the workspace as one ConfigMap value, so there is
// no state in which a reader sees half of a regeneration.
func TestReconcile_ClaudeMDIsASingleValue(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-md-one", "feature/md-one", "単一書き込み", "")

	var ws devplatformv1alpha1.Workspace
	if err := testClient.Get(ctx, types.NamespacedName{Name: "ws-md-one", Namespace: ns}, &ws); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	var cm corev1.ConfigMap
	if err := testClient.Get(ctx, types.NamespacedName{
		Name: ClaudeMDConfigMapName(ws.Status.WorkspaceId), Namespace: ns,
	}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if len(cm.Data) != 1 {
		t.Errorf("ConfigMap data has %d keys, want exactly %s", len(cm.Data), ClaudeMDKey)
	}
	if len(cm.OwnerReferences) == 0 {
		t.Error("ConfigMap has no owner reference, want it collected with its Workspace")
	}
}

// 10.6: the document is delivered as a mounted volume rather than written into
// the workspace by the control plane, which is what lets the supervisor decide
// when it takes effect.
func TestBuildStatefulSet_MountsClaudeMD(t *testing.T) {
	ws := &devplatformv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-md-mount", Namespace: "ns"},
		Spec:       devplatformv1alpha1.WorkspaceSpec{Branch: "feature/md-mount"},
	}
	tmpl := &devplatformv1alpha1.WorkspaceTemplate{
		Spec: devplatformv1alpha1.WorkspaceTemplateSpec{
			Image: "busybox:1.36",
			Resources: devplatformv1alpha1.WorkspaceResources{
				Requests: devplatformv1alpha1.ResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   devplatformv1alpha1.ResourceList{CPU: "500m", Memory: "256Mi"},
			},
		},
	}
	sts, err := buildStatefulSet(ws, tmpl, "ws-md-mount")
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}

	var mounted bool
	for _, m := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == blackboardVolumeName && m.MountPath == blackboardMountPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("volumeMounts = %+v, want the blackboard document mounted", sts.Spec.Template.Spec.Containers[0].VolumeMounts)
	}

	var sourced bool
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name != blackboardVolumeName {
			continue
		}
		if v.ConfigMap == nil || v.ConfigMap.Name != ClaudeMDConfigMapName("ws-md-mount") {
			t.Errorf("blackboard volume = %+v, want the workspace's CLAUDE.md ConfigMap", v)
		}
		// An absent ConfigMap must not wedge the Pod: the document is context,
		// not a precondition for running the branch.
		if v.ConfigMap != nil && (v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional) {
			t.Error("blackboard volume is not optional, want a missing document to leave the workspace startable")
		}
		sourced = true
	}
	if !sourced {
		t.Errorf("volumes = %+v, want a blackboard volume", sts.Spec.Template.Spec.Volumes)
	}

	var env string
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "BLACKBOARD_CLAUDE_MD" {
			env = e.Value
		}
	}
	if env != blackboardMountPath+"/"+ClaudeMDKey {
		t.Errorf("BLACKBOARD_CLAUDE_MD = %q, want the mounted document's path", env)
	}
}
