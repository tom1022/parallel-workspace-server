package session

import (
	"fmt"
	"time"
)

// EventAuthProbeFailed tells Hermes Agent this workspace never proved it can
// reach the model.
const EventAuthProbeFailed = "AuthProbeFailed"

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
	source := &Health{ConfigDir: health.ConfigDir, Source: "auth"}
	err := runProbe(sup, cfg)
	if err == nil {
		// A previous container's failed probe must not outlive a working one.
		source.ClearUnusable()
		return nil
	}

	detail := err.Error()
	health.MarkUnusable(detail)
	source.MarkUnusable(detail)
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
	time.Sleep(cfg.Settle)
	return Request(sup, "auth probe", cfg.Prompt, cfg.Timeout, cfg.Poll)
}

// Request delivers prompt to the session and returns once the turn it starts
// has completed. label names the caller in the errors it returns.
//
// It is the only way a process that is not attached gets a turn out of the
// session, so both the authentication probe and the self-healing loop go
// through here rather than each polling the transcript their own way.
func Request(sup *Supervisor, label, prompt string, timeout, poll time.Duration) error {
	// The config area survives on the PVC, so a previous container's finished
	// turn is already in the record. Without this snapshot the wait would read
	// that as this request's own completion and return immediately.
	before, err := sup.TurnState()
	if err != nil {
		return fmt.Errorf("supervisor: %s could not read the turn state: %w", label, err)
	}

	if err := sup.SendInput(prompt); err != nil {
		return fmt.Errorf("supervisor: %s could not reach the session: %w", label, err)
	}

	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		state, err := sup.TurnState()
		if err != nil {
			return fmt.Errorf("supervisor: %s could not read the turn state: %w", label, err)
		}

		switch state.Kind {
		case TurnCompleted:
			if state.EndedAt != before.EndedAt {
				return nil
			}
		case TurnFailed:
			// Same guard as the completed case: the record outlives the
			// container that wrote it, so an earlier failure is not this
			// request's. A crashed session reports no EndedAt at all, which
			// still differs from the snapshot and fails here.
			if state.EndedAt != before.EndedAt {
				return fmt.Errorf("supervisor: %s failed: %s", label, state.Detail)
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("supervisor: %s did not complete within %s (last state %q)", label, timeout, state.Kind)
		}
		time.Sleep(poll)
	}
}
