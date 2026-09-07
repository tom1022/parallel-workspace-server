// This file covers task 8.1: the per-branch Blackboard entry (design.md
// "Blackboard Reconciler" State Management, Requirement 10.1/10.2/10.3/10.8).
// The entry lives in Workspace status so the Workspace Controller remains its
// only writer and 10.9's serialization needs no lock of its own: each entry is
// written whole, in the single status update its reconcile pass was already
// making. The summary and public interfaces come in as annotations for the
// same reason browser connections do (connections.go) — the requester is
// outside the control plane and must not write status.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

const (
	// AnnotationBlackboardSummary carries the branch's work summary, stated by
	// whoever requested the workspace (10.2).
	AnnotationBlackboardSummary = "devplatform.fickledev.com/summary"
	// AnnotationBlackboardPublicInterfaces carries the comma-separated names
	// this branch exposes to the others (10.1).
	AnnotationBlackboardPublicInterfaces = "devplatform.fickledev.com/public-interfaces"
)

// changedFilesTimeout bounds one reconcile's read, for the same reason
// supervisorSSHCountTimeout does: the answer is only useful to the pass that
// asked for it.
const changedFilesTimeout = 5 * time.Second

// ChangedFilesReporter reports which files a workspace is currently changing.
type ChangedFilesReporter interface {
	ChangedFiles(ctx context.Context, ws *devplatformv1alpha1.Workspace) ([]string, error)
}

// SupervisorChangedFilesReporter reads the list the workspace Pod's Session
// Supervisor publishes. Like the other calls into the workspace runtime it
// addresses the Pod by IP, since workspaces have no Service of their own.
type SupervisorChangedFilesReporter struct {
	Client client.Client
	HTTP   *http.Client
	Port   int
}

func (s *SupervisorChangedFilesReporter) ChangedFiles(ctx context.Context, ws *devplatformv1alpha1.Workspace) ([]string, error) {
	ip, err := runningPodIP(ctx, s.Client, ws)
	if err != nil {
		// A workspace with no running Pod is changing nothing; that is an
		// answer, not a failure.
		return nil, nil
	}

	port := s.Port
	if port == 0 {
		port = supervisorPort
	}
	endpoint := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/changed-files"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("devplatform: changed files from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("devplatform: changed files from %s: %s", endpoint, resp.Status)
	}
	var body struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("devplatform: decoding changed files: %w", err)
	}
	return body.Files, nil
}

func (s *SupervisorChangedFilesReporter) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: changedFilesTimeout}
}

// syncBlackboard brings ws.Status.Blackboard up to date in place and reports
// whether anything changed. It deliberately does not write: the caller folds
// the result into the status update it is already making, which is what keeps
// a half-applied entry unobservable (10.9).
func (r *WorkspaceReconciler) syncBlackboard(ctx context.Context, ws *devplatformv1alpha1.Workspace) bool {
	entry := devplatformv1alpha1.BlackboardEntry{
		Branch:           ws.Spec.Branch,
		Summary:          ws.Annotations[AnnotationBlackboardSummary],
		PublicInterfaces: splitAnnotationList(ws.Annotations[AnnotationBlackboardPublicInterfaces]),
		Active:           true,
	}
	if ws.Status.Blackboard != nil {
		entry.ChangedFiles = ws.Status.Blackboard.ChangedFiles
	}
	if r.ChangedFilesReporter != nil {
		files, err := r.ChangedFilesReporter.ChangedFiles(ctx, ws)
		if err != nil {
			// The last known list is left standing: telling the other branches
			// that this one changed nothing is worse than telling them
			// something slightly stale.
			log.FromContext(ctx).Info("changed files unavailable, keeping the recorded list",
				"workspace", ws.Name, "error", err)
		} else {
			entry.ChangedFiles = files
		}
	}

	if ws.Status.Blackboard != nil && sameBlackboardContent(*ws.Status.Blackboard, entry) {
		return false
	}
	stamped := metav1.NewTime(r.now())
	entry.UpdatedAt = &stamped
	ws.Status.Blackboard = &entry
	return true
}

// deactivateBlackboard marks the branch's entry as no longer being worked on
// (10.8). The entry itself is kept: a reader that already saw the branch is
// better served by an explicit "finished" than by it vanishing.
func (r *WorkspaceReconciler) deactivateBlackboard(ws *devplatformv1alpha1.Workspace) bool {
	if ws.Status.Blackboard == nil || !ws.Status.Blackboard.Active {
		return false
	}
	ws.Status.Blackboard.Active = false
	stamped := metav1.NewTime(r.now())
	ws.Status.Blackboard.UpdatedAt = &stamped
	return true
}

func sameBlackboardContent(a, b devplatformv1alpha1.BlackboardEntry) bool {
	return a.Branch == b.Branch &&
		a.Summary == b.Summary &&
		a.Active == b.Active &&
		slices.Equal(a.ChangedFiles, b.ChangedFiles) &&
		slices.Equal(a.PublicInterfaces, b.PublicInterfaces)
}

func splitAnnotationList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ActiveBlackboardEntries returns the entries of the branches still being
// worked on in namespace, ordered by branch name. The order is fixed rather
// than incidental because 8.2 renders these into every workspace's CLAUDE.md
// and needs that rendering to be byte-identical across them.
func ActiveBlackboardEntries(ctx context.Context, c client.Reader, namespace string) ([]devplatformv1alpha1.BlackboardEntry, error) {
	var list devplatformv1alpha1.WorkspaceList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var entries []devplatformv1alpha1.BlackboardEntry
	for i := range list.Items {
		entry := list.Items[i].Status.Blackboard
		if entry == nil || !entry.Active || !list.Items[i].DeletionTimestamp.IsZero() {
			continue
		}
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Branch < entries[j].Branch })
	return entries, nil
}
