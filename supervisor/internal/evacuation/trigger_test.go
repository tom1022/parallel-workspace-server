package evacuation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testTrigger(t *testing.T, turn func() (bool, string, error)) (*Trigger, *fakeStore) {
	t.Helper()
	workingDir, _ := newRepo(t)
	store, fake := newFakeStore(t)
	return &Trigger{
		Agent: &Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store},
		Turn:  turn,
		Poll:  time.Millisecond,
		Wait:  200 * time.Millisecond,
	}, fake
}

func TestEvacuateWhenSettledWaitsForInFlightWrites(t *testing.T) {
	polls := 0
	trig, fake := testTrigger(t, func() (bool, string, error) {
		polls++
		return polls < 3, "", nil
	})

	if _, err := trig.EvacuateWhenSettled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if polls < 3 {
		t.Errorf("evacuated after %d polls; should have waited for the turn to finish", polls)
	}
	if len(fake.puts) != 2 {
		t.Errorf("puts = %v, want both objects written once", fake.puts)
	}
}

func TestEvacuateWhenSettledFailsRatherThanCaptureMidWrite(t *testing.T) {
	trig, fake := testTrigger(t, func() (bool, string, error) { return true, "", nil })

	if _, err := trig.EvacuateWhenSettled(context.Background()); err == nil {
		t.Fatal("expected an error when the turn never settles")
	}
	if len(fake.puts) != 0 {
		t.Errorf("uploaded %v despite writes still being in flight", fake.puts)
	}
}

func TestEvacuateWhenSettledPropagatesTurnErrors(t *testing.T) {
	trig, _ := testTrigger(t, func() (bool, string, error) { return false, "", errors.New("transcript unreadable") })
	if _, err := trig.EvacuateWhenSettled(context.Background()); err == nil {
		t.Fatal("expected the turn-state error to surface")
	}
}

func TestWatchTurnsEvacuatesOncePerCompletedTurn(t *testing.T) {
	completed := ""
	trig, fake := testTrigger(t, func() (bool, string, error) { return false, completed, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		trig.WatchTurns(ctx, func(error) {})
		close(done)
	}()

	waitForPuts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			fake.mu.Lock()
			n := len(fake.puts)
			fake.mu.Unlock()
			if n >= want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		t.Fatalf("puts = %v, want %d", fake.puts, want)
	}

	// A session that has never completed a turn must not evacuate.
	time.Sleep(20 * time.Millisecond)
	fake.mu.Lock()
	if len(fake.puts) != 0 {
		t.Errorf("evacuated %v before any turn completed", fake.puts)
	}
	fake.mu.Unlock()

	completed = "2026-09-05T12:00:00Z"
	waitForPuts(2)

	// The same completed turn observed again must not evacuate a second time.
	time.Sleep(20 * time.Millisecond)
	fake.mu.Lock()
	if len(fake.puts) != 2 {
		t.Errorf("puts = %v, want the same turn to trigger exactly one evacuation", fake.puts)
	}
	fake.mu.Unlock()

	completed = "2026-09-05T12:05:00Z"
	waitForPuts(4)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchTurns ignored context cancellation")
	}
}
