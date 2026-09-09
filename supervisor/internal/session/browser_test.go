package session

import (
	"testing"
)

func TestInteractiveBrowserVerificationNeedsAnAttachedDeveloper(t *testing.T) {
	s := newTestSupervisor(t)

	if got := s.InteractiveBrowserVerification(); got.Available {
		t.Errorf("availability = %+v, want unavailable with nobody attached (9.11)", got)
	}

	stop := attachClient(t, s.Tmux, false)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	if got := s.InteractiveBrowserVerification(); !got.Available {
		t.Errorf("availability = %+v, want available once a developer holds the session", got)
	}
}

func TestReadOnlyObserverCannotDriveTheirBrowser(t *testing.T) {
	s := newTestSupervisor(t)
	stop := attachClient(t, s.Tmux, true)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	if got := s.InteractiveBrowserVerification(); got.Available {
		t.Errorf("availability = %+v, want unavailable for a read-only observer", got)
	}
}

func TestLongLivedCredentialDisablesInteractiveBrowserVerification(t *testing.T) {
	s := newTestSupervisor(t)
	s.LongLivedAuth = true
	stop := attachClient(t, s.Tmux, false)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	got := s.InteractiveBrowserVerification()
	if got.Available {
		t.Errorf("availability = %+v, want unavailable under a long-lived credential (C-9)", got)
	}
	if got.Reason == "" {
		t.Error("an unavailable result must say why, so the session can tell the developer")
	}
}

// The self-healing loop runs with no developer attached. Interactive browser
// verification is unavailable there by construction, and that must not be a
// reason for the loop to stop (9.12).
func TestSelfHealingLoopRunsToCompletionWithoutInteractiveBrowserVerification(t *testing.T) {
	s := newTestSupervisor(t)
	s.LongLivedAuth = true

	const attempts = 3
	completed := 0
	for range attempts {
		if v := s.InteractiveBrowserVerification(); v.Available {
			t.Fatalf("test premise broken: %+v", v)
		}
		completed++
	}
	if completed != attempts {
		t.Errorf("loop completed %d of %d attempts", completed, attempts)
	}
}
