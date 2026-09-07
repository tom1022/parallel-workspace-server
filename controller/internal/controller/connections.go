// Developer connections as an idle-detection input (design.md "Workspace
// Controller" State Management: "status.lastActivityAt を Terminal Gateway の
// 接続イベントと Task Queue の実行状態から更新し" — 4.8 / 13.1). Both sources
// are read from here rather than written from outside: the Terminal Gateway
// records browser connections as an annotation on the Workspace, and the
// Session Supervisor only publishes its SSH session count for this loop to
// fetch. Neither writes Workspace status, which keeps the dependency running
// Control Plane -> Workspace Runtime and the controller the single status
// writer.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// AnnotationBrowserConnections carries the number of browser sessions the
// Terminal Gateway currently holds open against a workspace. It is an
// annotation, not a status field, because the gateway is outside the control
// plane and must not write status.
const AnnotationBrowserConnections = "devplatform.fickledev.com/browser-connections"

// supervisorSSHCountTimeout bounds one reconcile's read. It is short: the
// answer is only worth having while the reconcile that asked for it is still
// running.
const supervisorSSHCountTimeout = 5 * time.Second

// SSHSessionCounter reports how many SSH sessions currently hold a workspace.
type SSHSessionCounter interface {
	SSHSessionCount(ctx context.Context, ws *devplatformv1alpha1.Workspace) (int, error)
}

// SupervisorSSHSessionCounter reads the count the workspace Pod's Session
// Supervisor publishes. Like the evacuation requester it addresses the Pod by
// IP, since workspaces have no Service of their own.
type SupervisorSSHSessionCounter struct {
	Client client.Client
	HTTP   *http.Client
	Port   int
}

func (s *SupervisorSSHSessionCounter) SSHSessionCount(ctx context.Context, ws *devplatformv1alpha1.Workspace) (int, error) {
	ip, err := runningPodIP(ctx, s.Client, ws)
	if err != nil {
		// A workspace with no running Pod has no SSH sessions; that is an
		// answer, not a failure.
		return 0, nil
	}

	port := s.Port
	if port == 0 {
		port = supervisorPort
	}
	endpoint := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/ssh-sessions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("devplatform: ssh session count from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("devplatform: ssh session count from %s: %s", endpoint, resp.Status)
	}
	var body struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("devplatform: decoding ssh session count: %w", err)
	}
	return body.Count, nil
}

func (s *SupervisorSSHSessionCounter) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: supervisorSSHCountTimeout}
}

// connected reports whether a developer is holding the workspace right now,
// through either route (4.8).
func (r *WorkspaceReconciler) connected(ctx context.Context, ws *devplatformv1alpha1.Workspace) bool {
	if browserConnections(ws) > 0 {
		return true
	}
	if r.SSHSessionCounter == nil {
		return false
	}
	count, err := r.SSHSessionCounter.SSHSessionCount(ctx, ws)
	if err != nil {
		// Treated as "no sessions" so an unreachable supervisor cannot pin a
		// workspace Ready forever. The stop it may lead to is still gated on
		// a successful evacuation (16.8), so this costs uptime, never work.
		log.FromContext(ctx).Info("ssh session count unavailable, treating the workspace as unconnected",
			"workspace", ws.Name, "error", err)
		return false
	}
	return count > 0
}

func browserConnections(ws *devplatformv1alpha1.Workspace) int {
	n, err := strconv.Atoi(ws.Annotations[AnnotationBrowserConnections])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// runningPodIP resolves the workspace's Pod address for the control plane's
// synchronous calls into the workspace runtime.
func runningPodIP(ctx context.Context, c client.Client, ws *devplatformv1alpha1.Workspace) (string, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ws.Namespace), client.MatchingLabels(workspaceLabels(ws))); err != nil {
		return "", err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" && pod.DeletionTimestamp.IsZero() {
			return pod.Status.PodIP, nil
		}
	}
	return "", fmt.Errorf("devplatform: no running Pod for workspace %s", ws.Name)
}
