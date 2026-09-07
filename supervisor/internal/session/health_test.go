package session

import "testing"

// The database bootstrap marks the workspace unusable from the init container,
// a different process from the one that later serves /health (8.7). The mark
// therefore has to outlive the process that made it.
func TestHealth_UnusableMarkSurvivesTheProcessThatMadeIt(t *testing.T) {
	configDir := t.TempDir()

	(&Health{ConfigDir: configDir, Source: "dbboot"}).MarkUnusable("migration failed")

	usable, detail := (&Health{ConfigDir: configDir}).Status()
	if usable {
		t.Error("a fresh Health reports the workspace usable although it was marked unusable")
	}
	if detail != "migration failed" {
		t.Errorf("detail = %q, want the reason recorded by the other process", detail)
	}
}

func TestHealth_UsableWithoutAMark(t *testing.T) {
	usable, detail := (&Health{ConfigDir: t.TempDir()}).Status()
	if !usable || detail != "" {
		t.Errorf("Status() = (%v, %q), want an unmarked workspace to be usable", usable, detail)
	}
}

// A check that passes on a later start has to be able to withdraw its own
// verdict without withdrawing another check's.
func TestHealth_ClearingOneCheckLeavesAnotherMark(t *testing.T) {
	configDir := t.TempDir()
	(&Health{ConfigDir: configDir, Source: "dbboot"}).MarkUnusable("migration failed")
	(&Health{ConfigDir: configDir, Source: "auth"}).MarkUnusable("no credential")

	(&Health{ConfigDir: configDir, Source: "auth"}).ClearUnusable()

	usable, detail := (&Health{ConfigDir: configDir}).Status()
	if usable {
		t.Error("clearing the auth mark also withdrew the database bootstrap's")
	}
	if detail != "migration failed" {
		t.Errorf("detail = %q, want the mark that was not cleared", detail)
	}

	(&Health{ConfigDir: configDir, Source: "dbboot"}).ClearUnusable()
	if usable, _ := (&Health{ConfigDir: configDir}).Status(); !usable {
		t.Error("the workspace is still unusable after every mark was cleared")
	}
}
