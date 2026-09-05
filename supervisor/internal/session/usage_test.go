package session

import (
	"os"
	"path/filepath"
	"testing"
)

// Shape observed in Claude Code's own .claude.json. Only the fields the
// supervisor reads are reproduced.
const usageFixture = `{
  "cachedUsageUtilization": {
    "fetchedAtMs": 1788500671609,
    "utilization": {
      "limits": [
        {"kind":"session","group":"session","percent":33,"resets_at":"2026-09-04T07:50:00.439964+00:00","scope":null,"is_active":false},
        {"kind":"weekly_all","group":"weekly","percent":64,"resets_at":"2026-09-04T18:00:00.439983+00:00","scope":null,"is_active":true},
        {"kind":"weekly_scoped","group":"weekly","percent":10,"resets_at":null,"scope":{"model":{"display_name":"Opus"}},"is_active":false}
      ]
    }
  }
}`

func writeUsageState(t *testing.T, body string) string {
	t.Helper()
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configDir
}

func TestUsageReportsRemainingAndResetPerWindow(t *testing.T) {
	got, err := Usage(writeUsageState(t, usageFixture))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d snapshots, want 3: %+v", len(got), got)
	}

	session := got[0]
	if session.Kind != "session" || session.Group != "session" {
		t.Errorf("first snapshot = %+v, want the session window", session)
	}
	if session.UsedPercent != 33 || session.RemainingPercent != 67 {
		t.Errorf("session used/remaining = %v/%v, want 33/67", session.UsedPercent, session.RemainingPercent)
	}
	if session.ResetsAt != "2026-09-04T07:50:00.439964+00:00" {
		t.Errorf("session resetsAt = %q", session.ResetsAt)
	}

	if got[1].Kind != "weekly_all" || !got[1].Active {
		t.Errorf("weekly snapshot = %+v, want the active weekly window", got[1])
	}
	if got[2].Model != "Opus" {
		t.Errorf("model-scoped snapshot = %+v, want the model recorded so a per-model limit is distinguishable (7.6)", got[2])
	}
	if got[0].FetchedAt == "" {
		t.Error("the cache timestamp must be exposed; the caller cannot judge staleness without it")
	}
}

func TestUsageIsAbsentRatherThanFabricatedWhenClaudeHasNotCachedAny(t *testing.T) {
	got, err := Usage(writeUsageState(t, `{"numStartups":3}`))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no snapshots when Claude Code has published none", got)
	}
}

func TestUsageOnMissingStateIsNotAnError(t *testing.T) {
	got, err := Usage(t.TempDir())
	if err != nil {
		t.Fatalf("a workspace that has not run Claude Code yet is not a failure: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestUsageRejectsUnreadableState(t *testing.T) {
	if _, err := Usage(writeUsageState(t, "{not json")); err == nil {
		t.Error("expected an error rather than a silently empty quota reading")
	}
}
