// This file covers task 8.3: warning when several active branches are changing
// the same file (design.md "Blackboard Reconciler" Responsibilities,
// Requirement 10.7).
//
// Detection is per-workspace rather than namespace-wide so each side of a
// conflict hears about it in its own terms; the alternative — one pass
// announcing every pair — would tell nobody which of their files to leave
// alone.
package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// EventFileConflict names the warning sent when another active branch is
	// changing a file this one is changing too.
	EventFileConflict = "FileConflict"

	conditionFileConflict = "FileConflict"
)

// fileConflictDetail describes every file own shares with another active
// branch, or "" when there is nothing to warn about. own is passed separately
// from entries because the caller has just written it: the cached list can
// still be holding the previous revision of this branch's own entry.
func fileConflictDetail(entries []devplatformv1alpha1.BlackboardEntry, own *devplatformv1alpha1.BlackboardEntry) string {
	if own == nil || !own.Active || len(own.ChangedFiles) == 0 {
		return ""
	}

	merged := make([]devplatformv1alpha1.BlackboardEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.Branch != own.Branch {
			merged = append(merged, e)
		}
	}
	merged = append(merged, *own)
	// Fixed order so the detail of an unchanged conflict is byte-identical
	// between passes, which is what the repeat suppression below compares on.
	sort.Slice(merged, func(i, j int) bool { return merged[i].Branch < merged[j].Branch })

	branchesByFile := map[string][]string{}
	for _, e := range merged {
		seen := map[string]bool{}
		for _, f := range e.ChangedFiles {
			// A file listed twice by one branch must not read as two branches.
			if seen[f] {
				continue
			}
			seen[f] = true
			branchesByFile[f] = append(branchesByFile[f], e.Branch)
		}
	}

	var contested []string
	for f, branches := range branchesByFile {
		if len(branches) > 1 && slices.Contains(branches, own.Branch) {
			contested = append(contested, f)
		}
	}
	if len(contested) == 0 {
		return ""
	}
	sort.Strings(contested)

	parts := make([]string, 0, len(contested))
	for _, f := range contested {
		parts = append(parts, fmt.Sprintf("%q is being changed by %s", f, strings.Join(branchesByFile[f], ", ")))
	}
	return "file conflict: " + strings.Join(parts, "; ")
}

// reconcileFileConflicts announces a conflict once per distinct conflict. The
// recorded condition message is what marks one as already reported — the same
// idiom TaskQueueReconciler.stop uses, but persisted per workspace since the
// reconciler is shared across all of them.
func (r *WorkspaceReconciler) reconcileFileConflicts(ctx context.Context, ws *devplatformv1alpha1.Workspace) error {
	entries, err := ActiveBlackboardEntries(ctx, r.Client, ws.Namespace)
	if err != nil {
		return err
	}
	detail := fileConflictDetail(entries, ws.Status.Blackboard)
	cond := meta.FindStatusCondition(ws.Status.Conditions, conditionFileConflict)

	if detail == "" {
		if cond == nil || cond.Status == metav1.ConditionFalse {
			return nil
		}
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:               conditionFileConflict,
			Status:             metav1.ConditionFalse,
			Reason:             "NoOverlap",
			Message:            "no other active branch is changing these files",
			LastTransitionTime: metav1.NewTime(r.now()),
		})
		return r.Status().Update(ctx, ws)
	}

	if cond != nil && cond.Status == metav1.ConditionTrue && cond.Message == detail {
		return nil
	}
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               conditionFileConflict,
		Status:             metav1.ConditionTrue,
		Reason:             "SameFileChangedElsewhere",
		Message:            detail,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	if err := r.Status().Update(ctx, ws); err != nil {
		return err
	}
	r.notify(ctx, ws, EventFileConflict, detail)
	return nil
}
