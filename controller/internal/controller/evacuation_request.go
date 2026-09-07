// This file covers task 4.1's control-plane half: asking the Session
// Supervisor to evacuate before the execution environment it would capture
// from is stopped or torn down (design.md "Evacuation Agent" Dependencies:
// "Inbound: Workspace Controller — 中断・破棄の前段としての退避要求"), and
// confirming afterwards that a snapshot exists (16.4/16.8). The object store
// itself is never touched from here: the S3 credentials and client live only
// in the workspace Pod.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// supervisorEvacuateTimeout must outlast the supervisor's own settle wait for
// an executing turn (16.6), or this side gives up on an evacuation that is
// still going to succeed.
const supervisorEvacuateTimeout = 15 * time.Minute

// EvacuationRequester triggers an evacuation and returns the snapshot it
// produced. Failure means the working directory is NOT off-node, which is the
// caller's signal to leave the workspace running.
type EvacuationRequester interface {
	RequestEvacuation(ctx context.Context, ws *devplatformv1alpha1.Workspace) (*devplatformv1alpha1.EvacuationSnapshot, error)
}

// SupervisorEvacuationRequester posts to the workspace Pod's supervisor. It
// addresses the Pod by its IP rather than a Service name because no per-
// workspace Service exists yet (see ingress_reconciler.go), and a synchronous
// call does not outlive the Pod it is talking to anyway.
type SupervisorEvacuationRequester struct {
	Client client.Client
	HTTP   *http.Client
	Port   int
}

func (s *SupervisorEvacuationRequester) RequestEvacuation(ctx context.Context, ws *devplatformv1alpha1.Workspace) (*devplatformv1alpha1.EvacuationSnapshot, error) {
	ip, err := runningPodIP(ctx, s.Client, ws)
	if err != nil {
		return nil, err
	}

	port := s.Port
	if port == 0 {
		port = supervisorPort
	}
	endpoint := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/evacuate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(nil))
	if err != nil {
		return nil, err
	}

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("devplatform: evacuation request to %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("devplatform: evacuation request to %s: %s: %s", endpoint, resp.Status, body)
	}

	// The supervisor reports capturedAt as RFC3339, which metav1.Time decodes
	// directly.
	var snap devplatformv1alpha1.EvacuationSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("devplatform: decoding evacuation snapshot: %w", err)
	}
	return &snap, nil
}

func (s *SupervisorEvacuationRequester) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: supervisorEvacuateTimeout}
}

// StatusEvacuationConfirmer answers the destroy gate from the snapshot the
// controller itself recorded. status.lastEvacuation is written only after the
// supervisor confirmed the objects landed, so it is the durable record of "the
// working directory is off-node" that survives the Pod being gone by the time
// the PVC delete is due.
type StatusEvacuationConfirmer struct{}

func (StatusEvacuationConfirmer) IsEvacuationComplete(_ context.Context, ws *devplatformv1alpha1.Workspace) (bool, error) {
	return ws.Status.LastEvacuation != nil, nil
}

// HermesNotifier relays a condition needing human attention to Hermes Agent.
// An unset endpoint yields a no-op: chat relay is the platform's only
// notification path, and losing it must not stop reconciliation.
func HermesNotifier(endpoint string) func(context.Context, *devplatformv1alpha1.Workspace, string, string) {
	if endpoint == "" {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context, ws *devplatformv1alpha1.Workspace, kind, detail string) {
		body, err := json.Marshal(map[string]string{
			"kind":      kind,
			"workspace": ws.Name,
			"detail":    detail,
			"at":        time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		_ = resp.Body.Close()
	}
}
