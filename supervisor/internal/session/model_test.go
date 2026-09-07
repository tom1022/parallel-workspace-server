package session

import (
	"context"
	"strings"
	"testing"
	"time"
)

func staticTurn(state TurnState) func() (TurnState, error) {
	return func() (TurnState, error) { return state, nil }
}

func TestModelMatchesAcceptsAliasAndPinnedIdentifier(t *testing.T) {
	cases := []struct {
		expected, actual string
		want             bool
	}{
		{"claude-opus-5", "claude-opus-5", true},
		{"claude-opus-5", "claude-opus-5-20260101", true},
		{"opus", "claude-opus-5-20260101", true},
		{"Claude-Opus-5", "claude-opus-5-20260101", true},
		{"claude-opus-5", "claude-sonnet-5-20260101", false},
		{"opus", "claude-haiku-4-5-20251001", false},
	}
	for _, c := range cases {
		if got := modelMatches(c.expected, c.actual); got != c.want {
			t.Errorf("modelMatches(%q, %q) = %v, want %v", c.expected, c.actual, got, c.want)
		}
	}
}

func TestModelMonitorNotifiesOnDivergence(t *testing.T) {
	var events []Event
	mon := &ModelMonitor{
		Expected:  "claude-opus-5",
		Workspace: "feature-x",
		Turn:      staticTurn(TurnState{Kind: TurnCompleted, Model: "claude-sonnet-5-20260101"}),
		Notify:    func(e Event) { events = append(events, e) },
	}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != EventModelMismatch {
		t.Errorf("kind = %q, want %q", events[0].Kind, EventModelMismatch)
	}
	if events[0].Workspace != "feature-x" {
		t.Errorf("workspace = %q, want %q", events[0].Workspace, "feature-x")
	}
	// The operator has to be able to tell which way the switch went without
	// opening the workspace.
	if !strings.Contains(events[0].Detail, "claude-sonnet-5-20260101") || !strings.Contains(events[0].Detail, "claude-opus-5") {
		t.Errorf("detail %q must name both the observed and the configured model", events[0].Detail)
	}
}

func TestModelMonitorSilentWhenModelMatches(t *testing.T) {
	var events []Event
	mon := &ModelMonitor{
		Expected: "opus",
		Turn:     staticTurn(TurnState{Kind: TurnCompleted, Model: "claude-opus-5-20260101"}),
		Notify:   func(e Event) { events = append(events, e) },
	}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want none", len(events))
	}
}

func TestModelMonitorReportsOneSwitchOnce(t *testing.T) {
	var events []Event
	state := TurnState{Kind: TurnCompleted, Model: "claude-sonnet-5-20260101"}
	mon := &ModelMonitor{
		Expected: "claude-opus-5",
		Turn:     func() (TurnState, error) { return state, nil },
		Notify:   func(e Event) { events = append(events, e) },
	}
	for range 3 {
		if err := mon.Check(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if len(events) != 1 {
		t.Fatalf("got %d events for one switch, want 1", len(events))
	}

	// A turn that says nothing about the model must not re-arm the report:
	// every turn starts with such a state and the switch would notify again on
	// each one.
	state = TurnState{Kind: TurnRunning}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state = TurnState{Kind: TurnCompleted, Model: "claude-sonnet-5-20260101"}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want the switch reported once", len(events))
	}

	// Returning to the configured model re-arms it, so a later switch is a new
	// report rather than a silence.
	state = TurnState{Kind: TurnCompleted, Model: "claude-opus-5-20260101"}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state = TurnState{Kind: TurnCompleted, Model: "claude-sonnet-5-20260101"}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want the second switch reported", len(events))
	}
}

func TestModelMonitorIgnoresSyntheticModel(t *testing.T) {
	var events []Event
	mon := &ModelMonitor{
		Expected: "claude-opus-5",
		Turn:     staticTurn(TurnState{Kind: TurnCompleted, Model: "<synthetic>"}),
		Notify:   func(e Event) { events = append(events, e) },
	}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want none: a synthetic record ran on no model", len(events))
	}
}

func TestModelMonitorWithoutExpectationNeverReports(t *testing.T) {
	var events []Event
	mon := &ModelMonitor{
		Turn:   staticTurn(TurnState{Kind: TurnCompleted, Model: "claude-sonnet-5-20260101"}),
		Notify: func(e Event) { events = append(events, e) },
	}
	if err := mon.Check(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want none when no model is pinned", len(events))
	}
}

func TestModelMonitorWatchStopsWithContext(t *testing.T) {
	events := make(chan Event, 4)
	mon := &ModelMonitor{
		Expected: "claude-opus-5",
		Turn:     staticTurn(TurnState{Kind: TurnCompleted, Model: "claude-sonnet-5-20260101"}),
		Notify:   func(e Event) { events <- e },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		mon.Watch(ctx, time.Millisecond, nil)
		close(done)
	}()

	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("watch never reported the divergence")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return after the context ended")
	}
}
