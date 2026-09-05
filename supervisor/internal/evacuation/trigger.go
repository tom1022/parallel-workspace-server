package evacuation

import (
	"context"
	"fmt"
	"time"
)

// Trigger drives the two occasions an evacuation happens: a turn finishing on
// its own (16.7), and an explicit request that has to leave the working
// directory safe to stop (16.6/16.8).
type Trigger struct {
	Agent *Agent

	// Turn reports whether the session is mid-turn (writes may be in flight)
	// and an identifier that changes each time a turn completes. Supplied by
	// the caller so this package stays independent of the session's own
	// vocabulary of turn kinds.
	Turn func() (busy bool, completedAt string, err error)

	Poll time.Duration
	Wait time.Duration
}

// EvacuateWhenSettled waits out any turn still executing, then captures the
// working directory. It fails rather than capture a half-written tree: the
// caller's contract is to keep the workspace running when evacuation cannot
// complete, never to stop it unevacuated.
func (t *Trigger) EvacuateWhenSettled(ctx context.Context) (Snapshot, error) {
	deadline := time.Now().Add(t.Wait)
	for {
		busy, _, err := t.Turn()
		if err != nil {
			return Snapshot{}, err
		}
		if !busy {
			return t.Agent.Evacuate(ctx)
		}
		if !time.Now().Before(deadline) {
			return Snapshot{}, fmt.Errorf("evacuation: turn still running after %s; refusing to capture a partial working directory", t.Wait)
		}
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-time.After(t.Poll):
		}
	}
}

// WatchTurns evacuates once for each completed turn (16.7). The first observed
// value only establishes a baseline, so a session resuming with an already
// completed turn in its transcript does not re-evacuate work that is already
// off-node.
func (t *Trigger) WatchTurns(ctx context.Context, onError func(error)) {
	var seen string
	first := true
	ticker := time.NewTicker(t.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		_, completedAt, err := t.Turn()
		if err != nil {
			onError(err)
			continue
		}
		if first {
			first, seen = false, completedAt
			continue
		}
		if completedAt == "" || completedAt == seen {
			continue
		}
		seen = completedAt
		if _, err := t.Agent.Evacuate(ctx); err != nil {
			onError(err)
		}
	}
}
