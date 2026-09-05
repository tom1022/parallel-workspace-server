package session

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// EventAuthProbeFailed tells Hermes Agent this workspace never proved it can
// reach the model.
const EventAuthProbeFailed = "AuthProbeFailed"

// Health is the workspace's usability flag. It is separate from the turn state
// on purpose: a turn can fail and be retried, whereas an unusable workspace
// must not be handed work at all until an operator looks at it (5.8).
type Health struct {
	// ConfigDir is where the quota reading is served from.
	ConfigDir string

	mu       sync.RWMutex
	unusable string
}

// MarkUnusable records that the workspace must not be given work, with the
// reason. Chat relay is the platform's only push path and it can be down, so
// this durable, pollable mark — not the notification — is the real signal.
func (h *Health) MarkUnusable(detail string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unusable = detail
}

// Status reports whether the workspace may be handed work, and why not.
func (h *Health) Status() (bool, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.unusable == "", h.unusable
}

// Handler publishes usability and the quota reading to processes outside the
// workspace (5.8, 7.10).
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		usable, detail := h.Status()
		writeJSONResponse(w, http.StatusOK, map[string]any{"usable": usable, "detail": detail})
	})

	mux.HandleFunc("GET /usage", func(w http.ResponseWriter, r *http.Request) {
		snapshots, err := Usage(h.ConfigDir)
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if snapshots == nil {
			snapshots = []UsageSnapshot{}
		}
		writeJSONResponse(w, http.StatusOK, snapshots)
	})

	return mux
}

// ProbeConfig parameterises the one-shot authentication check.
type ProbeConfig struct {
	Prompt  string
	Timeout time.Duration
	Poll    time.Duration
	// Settle is how long to let the session's UI finish drawing before the
	// prompt is pasted. Input pasted into a TUI that has not finished starting
	// is dropped silently, and the probe issues exactly one request (5.7), so
	// there is no retry to fall back on. Tune it if a workspace's start is
	// slower than the default.
	Settle    time.Duration
	Workspace string
	HermesURL string
}

// AuthProbe confirms authentication works by putting one real request through
// the running session — the same route and session form a dispatched request
// takes (5.7). A batch invocation would prove less: it would authenticate a
// different process, with a different config area, that is not the thing tasks
// are actually sent to.
//
// Failure marks the workspace unusable and notifies Hermes Agent (5.8).
func AuthProbe(sup *Supervisor, health *Health, cfg ProbeConfig) error {
	err := runProbe(sup, cfg)
	if err == nil {
		return nil
	}

	detail := err.Error()
	health.MarkUnusable(detail)
	// Best-effort: Hermes Agent has no inbound endpoint of its own, so the
	// durable signal is the mark above.
	_ = NotifyHermes(cfg.HermesURL, Event{
		Kind:      EventAuthProbeFailed,
		Workspace: cfg.Workspace,
		Detail:    detail,
	})
	return err
}

func runProbe(sup *Supervisor, cfg ProbeConfig) error {
	// The config area survives on the PVC, so a previous container's finished
	// turn is already in the record. Without this snapshot the probe would
	// read that as its own success and never actually test the credential.
	before, err := sup.TurnState()
	if err != nil {
		return fmt.Errorf("supervisor: auth probe could not read the turn state: %w", err)
	}

	time.Sleep(cfg.Settle)
	if err := sup.SendInput(cfg.Prompt); err != nil {
		return fmt.Errorf("supervisor: auth probe could not reach the session: %w", err)
	}

	poll := cfg.Poll
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(cfg.Timeout)
	for {
		state, err := sup.TurnState()
		if err != nil {
			return fmt.Errorf("supervisor: auth probe could not read the turn state: %w", err)
		}

		switch state.Kind {
		case TurnCompleted:
			if state.EndedAt != before.EndedAt {
				return nil
			}
		case TurnFailed:
			return fmt.Errorf("supervisor: auth probe failed: %s", state.Detail)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("supervisor: auth probe did not complete within %s (last state %q)", cfg.Timeout, state.Kind)
		}
		time.Sleep(poll)
	}
}
