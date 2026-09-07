package session

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// EventModelMismatch tells Hermes Agent a turn ran on a model other than the
// one this workspace was configured with.
const EventModelMismatch = "ModelMismatch"

// syntheticModel is what Claude Code records for a message it composed itself
// instead of getting from the API. Such a record ran on no model at all.
const syntheticModel = "<synthetic>"

// ModelMonitor compares the model each turn actually ran on against the one
// the workspace was given.
//
// It is a safety net rather than a report: the credential's profile has been
// observed to advertise a plan other than the one actually held, so the
// control plane's model switching (7.7) can silently fail to take, and the
// only place that shows is the execution record.
type ModelMonitor struct {
	// Expected is the configured model. Empty disables the check — with
	// nothing pinned, Claude Code picks its own model and there is no
	// expectation to diverge from.
	Expected  string
	Workspace string
	Turn      func() (TurnState, error)
	Notify    func(Event)

	reported string
}

// Check reads the current turn and reports a model that does not answer to the
// expectation.
func (m *ModelMonitor) Check() error {
	if m.Expected == "" {
		return nil
	}
	state, err := m.Turn()
	if err != nil {
		return err
	}

	actual := state.Model
	// Silence rather than a reset: every turn passes through states that carry
	// no model, and treating those as agreement would re-report the same
	// switch on each turn that follows it.
	if actual == "" || actual == syntheticModel {
		return nil
	}
	if modelMatches(m.Expected, actual) {
		m.reported = ""
		return nil
	}
	if actual == m.reported {
		return nil
	}

	m.reported = actual
	if m.Notify != nil {
		m.Notify(Event{
			Kind:      EventModelMismatch,
			Workspace: m.Workspace,
			Detail:    fmt.Sprintf("turn ran on %q, configured model is %q", actual, m.Expected),
		})
	}
	return nil
}

// Watch runs Check on every tick until ctx ends.
func (m *ModelMonitor) Watch(ctx context.Context, poll time.Duration, onErr func(error)) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Check(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// modelMatches reports whether actual answers to the configured name. Claude
// Code takes both an alias ("opus") and a pinned identifier
// ("claude-opus-5-20260101") but the record always carries the resolved
// identifier, so equality alone would flag every turn of a correctly
// configured workspace.
//
// ponytail: containment either way. Tighten it if two model families ever
// share a name fragment.
func modelMatches(expected, actual string) bool {
	e := strings.ToLower(strings.TrimSpace(expected))
	a := strings.ToLower(strings.TrimSpace(actual))
	return strings.Contains(a, e) || strings.Contains(e, a)
}
