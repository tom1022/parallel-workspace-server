package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// conflictNote keeps the detail as well as the kind: 10.7 is specifically
// about what the warning says, so recordingNotifier (which drops it) is not
// enough here.
type conflictNote struct {
	kind   string
	detail string
}

func conflictNotifier(into *[]conflictNote) func(context.Context, *devplatformv1alpha1.Workspace, string, string) {
	return func(_ context.Context, _ *devplatformv1alpha1.Workspace, kind, detail string) {
		*into = append(*into, conflictNote{kind: kind, detail: detail})
	}
}

// reconcileFiles runs one Ready pass for a workspace whose supervisor reports
// files, recording any notification it produces.
func reconcileFiles(t *testing.T, ctx context.Context, ns, name string, files []string, into *[]conflictNote) {
	t.Helper()
	r := newTestReconciler()
	r.ChangedFilesReporter = &fakeChangedFilesReporter{files: files}
	if into != nil {
		r.Notify = conflictNotifier(into)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// 10.7: two active branches editing the same file is exactly what the
// Blackboard exists to surface, and the warning has to name the file and the
// branches involved for it to be actionable.
func TestReconcile_WarnsWhenTwoBranchesChangeTheSameFile(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-conflict-a", "feature/conflict-a", "認証基盤の刷新", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-conflict-b", "feature/conflict-b", "課金基盤の刷新", "")

	// a publishes first, so b's pass is the one that can see the overlap.
	reconcileFiles(t, ctx, ns, "ws-conflict-a", []string{"internal/shared.go", "internal/a.go"}, nil)

	var notes []conflictNote
	reconcileFiles(t, ctx, ns, "ws-conflict-b", []string{"internal/shared.go", "internal/b.go"}, &notes)

	if len(notes) != 1 {
		t.Fatalf("notifications = %v, want one file conflict warning", notes)
	}
	if notes[0].kind != EventFileConflict {
		t.Errorf("kind = %q, want %q", notes[0].kind, EventFileConflict)
	}
	for _, want := range []string{"internal/shared.go", "feature/conflict-a", "feature/conflict-b"} {
		if !strings.Contains(notes[0].detail, want) {
			t.Errorf("detail %q does not name %q", notes[0].detail, want)
		}
	}
	for _, unwanted := range []string{"internal/a.go", "internal/b.go"} {
		if strings.Contains(notes[0].detail, unwanted) {
			t.Errorf("detail %q names %q, which only one branch is changing", notes[0].detail, unwanted)
		}
	}
}

// Branches working in different files are the normal case and must stay
// silent, or the warning stops carrying information.
func TestReconcile_NoWarningWhenBranchesChangeDifferentFiles(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-apart-a", "feature/apart-a", "認証基盤の刷新", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-apart-b", "feature/apart-b", "課金基盤の刷新", "")

	reconcileFiles(t, ctx, ns, "ws-apart-a", []string{"internal/a.go"}, nil)

	var notes []conflictNote
	reconcileFiles(t, ctx, ns, "ws-apart-b", []string{"internal/b.go"}, &notes)

	if len(notes) != 0 {
		t.Fatalf("notifications = %v, want none", notes)
	}
}

// The same conflict must be announced once. Repeating it every pass would bury
// every other notification the platform sends.
func TestReconcile_DoesNotRepeatAnUnchangedConflict(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-repeat-a", "feature/repeat-a", "認証基盤の刷新", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-repeat-b", "feature/repeat-b", "課金基盤の刷新", "")

	reconcileFiles(t, ctx, ns, "ws-repeat-a", []string{"internal/shared.go"}, nil)

	var notes []conflictNote
	reconcileFiles(t, ctx, ns, "ws-repeat-b", []string{"internal/shared.go"}, &notes)
	reconcileFiles(t, ctx, ns, "ws-repeat-b", []string{"internal/shared.go"}, &notes)

	if len(notes) != 1 {
		t.Fatalf("notifications = %v, want the conflict announced once", notes)
	}
}

// A conflict that grows to a second file is new information, so it is
// announced again rather than suppressed as a repeat.
func TestReconcile_WarnsAgainWhenTheConflictChanges(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-grow-a", "feature/grow-a", "認証基盤の刷新", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-grow-b", "feature/grow-b", "課金基盤の刷新", "")

	reconcileFiles(t, ctx, ns, "ws-grow-a", []string{"internal/one.go", "internal/two.go"}, nil)

	var notes []conflictNote
	reconcileFiles(t, ctx, ns, "ws-grow-b", []string{"internal/one.go"}, &notes)
	reconcileFiles(t, ctx, ns, "ws-grow-b", []string{"internal/one.go", "internal/two.go"}, &notes)

	if len(notes) != 2 {
		t.Fatalf("notifications = %v, want the widened conflict announced too", notes)
	}
	if !strings.Contains(notes[1].detail, "internal/two.go") {
		t.Errorf("second detail %q does not name the file that joined the conflict", notes[1].detail)
	}
}

// A branch that stops touching the shared file clears the conflict, so the
// next one is announced instead of being mistaken for the old one.
func TestReconcile_ClearsTheConflictWhenTheOverlapEnds(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	createTemplate(t, ctx, ns, "default")
	provisionToReadyWithSummary(t, ctx, ns, "ws-clear-a", "feature/clear-a", "認証基盤の刷新", "")
	provisionToReadyWithSummary(t, ctx, ns, "ws-clear-b", "feature/clear-b", "課金基盤の刷新", "")

	reconcileFiles(t, ctx, ns, "ws-clear-a", []string{"internal/shared.go"}, nil)

	var notes []conflictNote
	reconcileFiles(t, ctx, ns, "ws-clear-b", []string{"internal/shared.go"}, &notes)
	reconcileFiles(t, ctx, ns, "ws-clear-b", []string{"internal/b.go"}, &notes)
	reconcileFiles(t, ctx, ns, "ws-clear-b", []string{"internal/shared.go"}, &notes)

	if len(notes) != 2 {
		t.Fatalf("notifications = %v, want the conflict announced on each time it appeared", notes)
	}
}

// An inactive branch is not being worked on, so its recorded files are history
// rather than a conflict (10.8 feeding 10.7).
func TestFileConflictDetail_IgnoresInactiveBranches(t *testing.T) {
	own := &devplatformv1alpha1.BlackboardEntry{
		Branch:       "feature/live",
		ChangedFiles: []string{"internal/shared.go"},
		Active:       true,
	}
	entries := []devplatformv1alpha1.BlackboardEntry{*own}

	if detail := fileConflictDetail(entries, own); detail != "" {
		t.Errorf("detail = %q, want none when only this branch is active", detail)
	}
}
