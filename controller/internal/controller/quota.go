// Quota observation for the Task Queue and Quota Governor (7.5, 7.6, 7.11).
// Everything the dispatch decision rests on is read from here, and everything
// read from here is a typed field: the quota windows Claude Code publishes as
// its own local state, and the API's error type for a failed turn. The
// rendered message the supervisor also serves is deliberately not decoded, so
// no wording change can move the decision.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	devplatformv1alpha1 "github.com/tom1022/gitops-apps/apps/devplatform/controller/api/v1alpha1"
)

// Notification kinds raised by the governor.
const (
	EventQuotaExhausted = "QuotaExhausted"
	EventAuthError      = "AuthError"
)

// AnnotationActiveModel carries the model a workspace was switched to after
// its model-scoped window ran out (7.7). It overrides the template's model.
// An annotation, not a status field, so the governor can record it without
// contending with the Workspace reconciler over status ownership.
const AnnotationActiveModel = "devplatform.fickledev.com/active-model"

// usageReadTimeout bounds one reconcile's read, matching the other synchronous
// calls the control plane makes into a workspace.
const usageReadTimeout = 5 * time.Second

// UsageSnapshot mirrors one entry of the Session Supervisor's /usage payload
// (design.md: "利用枠情報は UsageSnapshot の配列として提供する"). The supervisor
// is a separate module, so the shape is restated rather than imported.
type UsageSnapshot struct {
	Kind             string  `json:"kind"`
	Group            string  `json:"group"`
	RemainingPercent float64 `json:"remainingPercent"`
	ResetsAt         string  `json:"resetsAt,omitempty"`
	// Model is set only on a window scoped to one model, which is what makes
	// that window survivable by switching model (7.7).
	Model  string `json:"model,omitempty"`
	Active bool   `json:"active"`
}

// UsageReading is one observation of a workspace.
type UsageReading struct {
	Snapshots []UsageSnapshot
	// ErrorKind is the Anthropic API's error type for the workspace's last
	// turn, empty when the turn did not fail.
	ErrorKind string
}

// UsageObserver reports a workspace's quota reading and last error kind.
type UsageObserver interface {
	ObserveUsage(ctx context.Context, ws *devplatformv1alpha1.Workspace) (UsageReading, error)
}

// SupervisorUsageObserver reads both from the workspace Pod's Session
// Supervisor. Like the other calls into a workspace it addresses the Pod by
// IP, since workspaces have no Service of their own.
type SupervisorUsageObserver struct {
	Client client.Client
	HTTP   *http.Client
	Port   int
}

func (o *SupervisorUsageObserver) ObserveUsage(ctx context.Context, ws *devplatformv1alpha1.Workspace) (UsageReading, error) {
	ip, err := runningPodIP(ctx, o.Client, ws)
	if err != nil {
		return UsageReading{}, err
	}
	return o.observe(ctx, ip)
}

func (o *SupervisorUsageObserver) observe(ctx context.Context, ip string) (UsageReading, error) {
	port := o.Port
	if port == 0 {
		port = supervisorPort
	}
	base := "http://" + net.JoinHostPort(ip, strconv.Itoa(port))

	var reading UsageReading
	if err := o.get(ctx, base+"/usage", &reading.Snapshots); err != nil {
		return UsageReading{}, err
	}
	// Only errorKind is decoded. The turn state carries a rendered detail too,
	// and leaving it out of this struct is what makes 7.5 checkable rather
	// than merely intended.
	var turn struct {
		ErrorKind string `json:"errorKind"`
	}
	if err := o.get(ctx, base+"/turn", &turn); err != nil {
		return UsageReading{}, err
	}
	reading.ErrorKind = turn.ErrorKind
	return reading, nil
}

func (o *SupervisorUsageObserver) get(ctx context.Context, endpoint string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	c := o.HTTP
	if c == nil {
		c = &http.Client{Timeout: usageReadTimeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("devplatform: reading %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("devplatform: reading %s: %s", endpoint, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("devplatform: decoding %s: %w", endpoint, err)
	}
	return nil
}

// exhaustedScope reports the widest quota window that is out of balance.
// Widest wins: a model-scoped window can be escaped by switching model (7.7),
// an account-wide one cannot, so reporting the narrow scope while the wide one
// is also gone would keep dispatching into a wall.
func exhaustedScope(snaps []UsageSnapshot, minRemainingPercent float64) (QuotaScope, bool) {
	var scope QuotaScope
	for _, s := range snaps {
		// An inactive window is not a limit currently in effect; treating one
		// as exhausted would stop dispatch on a limit that does not apply.
		if !s.Active || s.RemainingPercent > minRemainingPercent {
			continue
		}
		switch {
		case s.Model != "":
			if scope == "" {
				scope = QuotaScopeModel
			}
		case s.Group == groupWeekly:
			scope = QuotaScopeWeekly
		default:
			return QuotaScopeSession, true
		}
	}
	return scope, scope != ""
}

// groupWeekly is Claude Code's own label for the seven-day windows; the
// session window carries a different one.
const groupWeekly = "weekly"

// isAuthError reports whether an API error type means the credential itself is
// the problem, which stops every workspace rather than throttling one (7.9).
// A rate limit is not one of these: that is a quota condition and is judged
// from the reading instead.
func isAuthError(kind string) bool {
	return kind == "authentication_error" || kind == "permission_error"
}
